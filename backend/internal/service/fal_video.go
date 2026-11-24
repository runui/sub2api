package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideotask"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	falVideoIDPrefix        = "falvid_"
	falVideoTextEndpoint    = falVideoUpstreamModel + "/text-to-video"
	falVideoImageEndpoint   = falVideoUpstreamModel + "/image-to-video"
	falVideoUpstreamModel   = "xai/grok-imagine-video/v1.5"
	falVideoHTTPTimeout     = 20 * time.Second
	falVideoPollInterval    = 5 * time.Second
	falVideoJobDeadline     = 2 * time.Hour
	falVideoMaxContentBytes = 512 << 20
)

var falVideoContentClient = &http.Client{
	Transport:     newImageDownloadBaseTransport(imageDownloadDialControl),
	Timeout:       2 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var (
	ErrFalVideoInvalid                = errors.New("invalid fal video request")
	ErrFalVideoNotFound               = errors.New("video request not found")
	ErrFalVideoConflict               = errors.New("idempotency key already used for a different request")
	ErrFalVideoSubmitting             = errors.New("video submission is still being recorded; check the request status")
	ErrFalVideoUpstream               = errors.New("fal video upstream request failed")
	ErrFalVideoSubmissionUnknown      = errors.New("fal video submission outcome unknown; do not resubmit automatically")
	ErrFalVideoTemporarilyUnavailable = errors.New("fal video is temporarily unavailable")
	ErrFalVideoInsufficientBalance    = errors.New("insufficient balance for video generation")
	ErrFalVideoKeyLimit               = errors.New("API key spending limit would be exceeded by video generation")
)

type FalVideoRequest struct {
	UserID, APIKeyID, GroupID, SubscriptionID                        int64
	Model, Prompt, ImageURL, Resolution, AspectRatio, IdempotencyKey string
	RoutingModel                                                     string
	Duration                                                         int
	APIKey                                                           *APIKey
	Subscription                                                     *UserSubscription
}

type FalVideoBilling interface {
	Reserve(context.Context, falvideotask.Task) error
	Settle(context.Context, falvideotask.Task, bool) error
}

type falVideoStore interface {
	Create(context.Context, falvideotask.Task) (falvideotask.Task, bool, error)
	Get(context.Context, string) (falvideotask.Task, error)
	GetByIdempotencyKey(context.Context, int64, int64, int64, string) (falvideotask.Task, error)
	Accept(context.Context, string, []byte) error
	Reject(context.Context, string, string, string) error
	Claim(context.Context, int, time.Duration) ([]falvideotask.Task, error)
	Reschedule(context.Context, string, string, string, time.Time) error
	Finish(context.Context, string, string, string, string, float64, int, int, string) error
	MarkStaleSubmissions(context.Context, time.Time) error
}

type FalVideoService struct {
	store       falVideoStore
	accounts    AccountRepository
	transport   HTTPUpstream
	contentDo   func(*http.Request) (*http.Response, error)
	billing     FalVideoBilling
	usage       UsageLogRepository
	cache       *BillingCacheService
	keys        *APIKeyService
	concurrency *ConcurrencyService
	gateway     *GatewayService
	stop        context.CancelFunc
	worker      sync.WaitGroup
}

func ProvideFalVideoService(db *sql.DB, accounts AccountRepository, transport HTTPUpstream,
	billing FalVideoBilling, usage UsageLogRepository, cache *BillingCacheService, keys *APIKeyService, concurrency *ConcurrencyService, gateway *GatewayService) *FalVideoService {
	s := &FalVideoService{store: falvideotask.NewStore(db), accounts: accounts,
		transport: transport, billing: billing, usage: usage, cache: cache, keys: keys, concurrency: concurrency, gateway: gateway}
	// Recover persisted tasks independently of current model routing.
	ctx, stop := context.WithCancel(context.Background())
	s.stop = stop
	s.worker.Add(1)
	go s.run(ctx)
	return s
}

func (s *FalVideoService) Stop() {
	if s != nil && s.stop != nil {
		s.stop()
		s.worker.Wait()
	}
}

func IsFalVideoTaskID(id string) bool { return strings.HasPrefix(id, falVideoIDPrefix) }

func falVideoAccountModel(account *Account, model string) (string, bool) {
	if account == nil || account.Platform != PlatformFal || account.Type != AccountTypeAPIKey || !hasFalVideoAPIKey(account.Credentials) || !explicitModelMappingClaims(*account, model) {
		return "", false
	}
	upstream := strings.TrimSpace(account.GetMappedModel(model))
	return upstream, upstream == falVideoUpstreamModel
}

func falVideoInput(req FalVideoRequest) (falvideo.Input, error) {
	input := falvideo.Input{Prompt: req.Prompt, ImageURL: req.ImageURL, Duration: req.Duration, Resolution: req.Resolution, AspectRatio: req.AspectRatio}
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 32<<10 || len(req.IdempotencyKey) > 128 || strings.ContainsAny(req.IdempotencyKey, "\r\n\x00") {
		return input, fmt.Errorf("%w: invalid prompt or Idempotency-Key", ErrFalVideoInvalid)
	}
	if input.Duration == 0 {
		input.Duration = 6
	}
	if input.Duration < 1 || input.Duration > 15 {
		return input, fmt.Errorf("%w: duration must be 1 to 15 seconds", ErrFalVideoInvalid)
	}
	if input.Resolution == "" {
		input.Resolution = "720p"
	}
	switch input.Resolution {
	case "480p", "720p", "1080p":
	default:
		return input, fmt.Errorf("%w: unsupported resolution", ErrFalVideoInvalid)
	}
	if input.ImageURL != "" {
		u, err := url.Parse(input.ImageURL)
		if err != nil || len(input.ImageURL) > 8192 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
			return input, fmt.Errorf("%w: image must be an HTTPS URL", ErrFalVideoInvalid)
		}
		if input.AspectRatio != "" {
			return input, fmt.Errorf("%w: aspect_ratio is not supported with image input", ErrFalVideoInvalid)
		}
	} else {
		switch input.AspectRatio {
		case "", "16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16", "21:9", "5:2":
		default:
			return input, fmt.Errorf("%w: unsupported aspect_ratio", ErrFalVideoInvalid)
		}
		if input.AspectRatio == "" {
			input.AspectRatio = "16:9"
		}
	}
	return input, nil
}

func (s *FalVideoService) Replay(ctx context.Context, req FalVideoRequest) (falvideotask.Task, bool, error) {
	var empty falvideotask.Task
	if req.IdempotencyKey == "" {
		return empty, false, nil
	}
	task, err := s.store.GetByIdempotencyKey(ctx, req.UserID, req.APIKeyID, req.GroupID, req.IdempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, ErrFalVideoTemporarilyUnavailable
	}
	input, err := falVideoInput(req)
	if err != nil || task.Fingerprint != falVideoFingerprint(req.Model, input) {
		return empty, false, ErrFalVideoConflict
	}
	return task, true, falVideoReplayError(task)
}

func falVideoReplayError(task falvideotask.Task) error {
	if task.State == "submitting" {
		return ErrFalVideoSubmitting
	}
	return nil
}

func falVideoFingerprint(model string, input falvideo.Input) string {
	payload, _ := json.Marshal(input) // Input consists exclusively of strings and integers.
	// Idempotency describes the client input, not current routing or pricing.
	hash := sha256.Sum256(append([]byte(model+"\x00"), payload...))
	return hex.EncodeToString(hash[:])
}

func (s *FalVideoService) Submit(ctx context.Context, req FalVideoRequest) (falvideotask.Task, error) {
	var empty falvideotask.Task
	if req.GroupID <= 0 || req.UserID <= 0 || req.APIKeyID <= 0 || req.APIKey == nil || req.APIKey.Group == nil || req.APIKey.Group.ID != req.GroupID {
		return empty, ErrFalVideoNotFound
	}
	if req.SubscriptionID != 0 || req.Subscription != nil || req.APIKey.Group.IsSubscriptionType() {
		return empty, fmt.Errorf("%w: fal video requires a balance-billed group", ErrFalVideoInvalid)
	}
	if replay, found, err := s.Replay(ctx, req); found || err != nil {
		return replay, err
	}
	input, err := falVideoInput(req)
	if err != nil {
		return empty, err
	}
	routingModel := firstNonEmpty(req.RoutingModel, req.Model)
	group := req.APIKey.Group
	// An arbitrary routing alias still prices against the fal adapter's model
	// family. Freeze the effective rate before reserving, just like the duration.
	price := group.GetVideoPriceForModel(VideoPriceFamilyGrokImagineVideo15, input.Resolution)
	multiplier := group.RateMultiplier
	if s.gateway != nil && !group.VideoRateIndependent {
		multiplier = s.gateway.ResolveUserGroupRateMultiplier(ctx, req.UserID, req.GroupID, multiplier)
	}
	unitPrice := 0.0
	if price != nil {
		unitPrice = *price * resolveVideoRateMultiplier(req.APIKey, multiplier)
	}
	amount := QuantizeUsageBillingAmount(unitPrice * float64(input.Duration))
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return empty, fmt.Errorf("%w: configure a positive video price for this model and resolution", ErrFalVideoInvalid)
	}
	account, endpoint, release, err := s.selectAccount(ctx, req.GroupID, routingModel, input.ImageURL != "")
	if err != nil {
		return empty, err
	}
	defer release()
	fingerprint := falVideoFingerprint(req.Model, input)
	task := falvideotask.Task{ID: falVideoIDPrefix + uuid.NewString(), Model: req.Model, Endpoint: endpoint, Resolution: input.Resolution,
		UserID: req.UserID, APIKeyID: req.APIKeyID, GroupID: req.GroupID, AccountID: account.ID,
		Duration: input.Duration, UnitPriceUSD: unitPrice, IdempotencyKey: req.IdempotencyKey, Fingerprint: fingerprint}
	task, created, err := s.store.Create(ctx, task)
	if err != nil {
		return empty, ErrFalVideoTemporarilyUnavailable
	}
	if !created {
		if task.Fingerprint != fingerprint {
			return empty, ErrFalVideoConflict
		}
		return task, falVideoReplayError(task)
	}

	// A disconnected downstream must not prevent recording the POST outcome.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*falVideoHTTPTimeout)
	defer cancel()
	if err := s.billing.Reserve(work, task); err != nil {
		s.reject(work, task.ID, "reservation_failed")
		if errors.Is(err, ErrBatchImageInsufficientBalance) {
			return task, ErrFalVideoInsufficientBalance
		}
		if errors.Is(err, ErrFalVideoKeyLimit) {
			return task, ErrFalVideoKeyLimit
		}
		return task, ErrFalVideoTemporarilyUnavailable
	}
	s.invalidateBalance(work, task.UserID)
	requestCtx, stop := context.WithTimeout(work, falVideoHTTPTimeout)
	ref, err := s.client(account).Submit(requestCtx, account.GetCredential("api_key"), endpoint, input)
	stop()
	if err != nil {
		var upstream *falvideo.Error
		if errors.As(err, &upstream) && upstream.StatusCode >= 400 && upstream.StatusCode < 500 && upstream.StatusCode != http.StatusRequestTimeout {
			s.reject(work, task.ID, "upstream_rejected")
			if upstream.StatusCode == http.StatusUnprocessableEntity || upstream.StatusCode == http.StatusBadRequest {
				return task, fmt.Errorf("%w: fal rejected the input", ErrFalVideoInvalid)
			}
			return task, ErrFalVideoUpstream
		}
		// A failed response does not prove the upstream did not enqueue the job.
		s.reject(work, task.ID, "submission_unknown")
		return task, ErrFalVideoSubmissionUnknown
	}
	refs, err := json.Marshal(ref)
	if err != nil || s.store.Accept(work, task.ID, refs) != nil {
		logger.L().Error("fal_video.accept_persist_failed", zap.String("task_id", task.ID), zap.String("fal_request_id", ref.RequestID))
		return task, ErrFalVideoSubmissionUnknown
	}
	task.State = "queued"
	return task, nil
}

func (s *FalVideoService) selectAccount(ctx context.Context, groupID int64, model string, hasImage bool) (*Account, string, func(), error) {
	accounts, err := s.accounts.ListSchedulableByGroupIDAndPlatform(ctx, groupID, PlatformFal)
	if err != nil {
		return nil, "", nil, ErrFalVideoTemporarilyUnavailable
	}
	// The repository orders by group priority, then account priority. Only
	// fall through to another account before any task or upstream side effect.
	for i := range accounts {
		account := &accounts[i]
		upstream, ok := falVideoAccountModel(account, model)
		if !ok || !account.IsSchedulable() {
			continue
		}
		// The mapped model was checked against the adapter's allowlist. Never
		// interpret a client model name as an arbitrary fal queue path.
		endpoint := upstream + "/text-to-video"
		if hasImage {
			endpoint = upstream + "/image-to-video"
		}
		if s.concurrency == nil {
			return account, endpoint, func() {}, nil
		}
		slot, err := s.concurrency.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if err != nil {
			return nil, "", nil, ErrFalVideoTemporarilyUnavailable
		}
		if slot != nil && slot.Acquired {
			return account, endpoint, slot.ReleaseFunc, nil
		}
	}
	return nil, "", nil, ErrFalVideoTemporarilyUnavailable
}

func (s *FalVideoService) reject(ctx context.Context, id, code string) {
	if err := s.store.Reject(ctx, id, "pending_failure", code); err != nil {
		logger.L().Warn("fal_video.reject_persist_failed", zap.String("task_id", id), zap.String("code", code))
	}
}

func (s *FalVideoService) Get(ctx context.Context, id string, userID, keyID, groupID int64) (falvideotask.Task, error) {
	task, err := s.store.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (task.UserID != userID || task.APIKeyID != keyID || task.GroupID != groupID)) {
		return falvideotask.Task{}, ErrFalVideoNotFound
	}
	if err != nil {
		return falvideotask.Task{}, ErrFalVideoTemporarilyUnavailable
	}
	return task, nil
}

func (s *FalVideoService) account(ctx context.Context, id int64) (*Account, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Platform != PlatformFal || a.Type != AccountTypeAPIKey || !hasFalVideoAPIKey(a.Credentials) {
		return nil, ErrFalVideoTemporarilyUnavailable
	}
	return a, nil
}

func hasFalVideoAPIKey(credentials map[string]any) bool {
	key, ok := credentials["api_key"].(string)
	return ok && key != "" && !strings.ContainsFunc(key, func(r rune) bool { return r <= ' ' || r >= 127 })
}

func (s *FalVideoService) client(account *Account) *falvideo.Client {
	return falvideo.NewClient(func(req *http.Request) (*http.Response, error) {
		ctx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(req.Context()))
		proxy := ""
		if account.Proxy != nil {
			proxy = account.Proxy.URL()
		}
		return s.transport.Do(req.WithContext(ctx), proxy, account.ID, account.Concurrency)
	})
}

func falVideoCDN(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	return u, host == "fal.media" || strings.HasSuffix(host, ".fal.media")
}

func (s *FalVideoService) OpenContent(ctx context.Context, task falvideotask.Task, rangeHeader string) (*http.Response, error) {
	if task.State != "succeeded" {
		return nil, ErrFalVideoConflict
	}
	u, ok := falVideoCDN(task.ResultURL)
	if !ok {
		return nil, ErrFalVideoUpstream
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(ctx)), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrFalVideoUpstream
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	do := s.contentDo
	if do == nil {
		do = falVideoContentClient.Do
	}
	resp, err := do(req)
	if err != nil {
		return nil, ErrFalVideoUpstream
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		if resp.ContentLength > falVideoMaxContentBytes {
			_ = resp.Body.Close()
			return nil, ErrFalVideoUpstream
		}
		resp.Body = http.MaxBytesReader(nil, resp.Body, falVideoMaxContentBytes)
		return resp, nil
	case http.StatusRequestedRangeNotSatisfiable:
		return resp, nil
	case http.StatusNotFound, http.StatusGone:
		_ = resp.Body.Close()
		return nil, ErrFalVideoNotFound
	default:
		_ = resp.Body.Close()
		return nil, ErrFalVideoUpstream
	}
}

func (s *FalVideoService) run(ctx context.Context) {
	defer s.worker.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			logger.L().Warn("fal_video.worker_failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *FalVideoService) runOnce(ctx context.Context) error {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := s.store.MarkStaleSubmissions(dbCtx, time.Now().Add(-2*time.Minute)); err != nil {
		cancel()
		return err
	}
	tasks, err := s.store.Claim(dbCtx, 8, 2*time.Minute)
	cancel()
	if err != nil {
		return err
	}
	var jobs sync.WaitGroup
	for _, task := range tasks {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			work, stop := context.WithTimeout(ctx, time.Minute)
			defer stop()
			if err := s.advance(work, task); err != nil && ctx.Err() == nil {
				logger.L().Warn("fal_video.advance_failed", zap.String("task_id", task.ID), zap.String("state", task.State), zap.Error(err))
			}
		}()
	}
	jobs.Wait()
	return nil
}

func (s *FalVideoService) advance(ctx context.Context, t falvideotask.Task) error {
	finish := func(state, code string, r falvideo.Result) error {
		return s.store.Finish(ctx, t.ID, t.LeaseToken, state, r.URL, r.Duration, r.Width, r.Height, code)
	}
	reschedule := func(state string, delay time.Duration) error {
		return s.store.Reschedule(ctx, t.ID, t.LeaseToken, state, time.Now().Add(delay))
	}
	if t.State == "pending_success" || t.State == "pending_failure" {
		success := t.State == "pending_success"
		if err := s.settle(ctx, t, success); err != nil {
			return errors.Join(err, reschedule(t.State, 30*time.Second))
		}
		state := "failed"
		if success {
			state = "succeeded"
		} else if t.ErrorCode == "submission_unknown" {
			state = "unknown"
		}
		return finish(state, t.ErrorCode, falvideo.Result{URL: t.ResultURL, Duration: t.ActualDuration, Width: t.Width, Height: t.Height})
	}
	if time.Since(t.CreatedAt) > falVideoJobDeadline {
		return finish("pending_failure", "result_timeout", falvideo.Result{})
	}
	account, err := s.account(ctx, t.AccountID)
	if err != nil {
		return reschedule(t.State, 30*time.Second)
	}
	var ref falvideo.Reference
	if err := json.Unmarshal(t.Reference, &ref); err != nil {
		return finish("pending_failure", "invalid_queue_reference", falvideo.Result{})
	}
	client, key := s.client(account), account.GetCredential("api_key")
	call, cancel := context.WithTimeout(ctx, falVideoHTTPTimeout)
	status, err := client.Status(call, key, ref)
	cancel()
	if err != nil {
		return reschedule(t.State, 15*time.Second)
	}
	switch status {
	case falvideo.StatusInQueue:
		return reschedule("queued", falVideoPollInterval)
	case falvideo.StatusInProgress:
		return reschedule("running", falVideoPollInterval)
	case falvideo.StatusCompleted:
		call, cancel := context.WithTimeout(ctx, falVideoHTTPTimeout)
		result, err := client.Result(call, key, ref)
		cancel()
		if err != nil {
			var upstream *falvideo.Error
			if errors.As(err, &upstream) {
				switch upstream.StatusCode {
				case 400, 422, 500, 504:
					return finish("pending_failure", "generation_failed", falvideo.Result{})
				case 404:
					return finish("pending_failure", "result_unavailable", falvideo.Result{})
				}
			}
			return reschedule("running", 15*time.Second)
		}
		if _, ok := falVideoCDN(result.URL); !ok {
			return finish("pending_failure", "invalid_video_output", falvideo.Result{})
		}
		if result.Duration <= 0 || math.IsNaN(result.Duration) || math.IsInf(result.Duration, 0) {
			result.Duration = float64(t.Duration)
		}
		return finish("pending_success", "", result)
	default:
		return reschedule(t.State, 15*time.Second)
	}
}

func (s *FalVideoService) settle(ctx context.Context, t falvideotask.Task, success bool) error {
	if err := s.billing.Settle(ctx, t, success); err != nil {
		return err
	}
	s.invalidateBalance(ctx, t.UserID)
	if success && s.cache != nil {
		_ = s.cache.InvalidateAPIKeyRateLimit(ctx, t.APIKeyID)
	}
	if !success {
		return nil
	}
	cost := QuantizeUsageBillingAmount(t.UnitPriceUSD * float64(t.Duration))
	mode, media, path := string(BillingModeVideo), "video", "/v1/videos/generations"
	log := &UsageLog{UserID: t.UserID, APIKeyID: t.APIKeyID, AccountID: t.AccountID, GroupID: &t.GroupID,
		RequestID: t.ID, Model: t.Model, RequestedModel: t.Model, UpstreamModel: &t.Endpoint, InboundEndpoint: &path,
		UpstreamEndpoint: &t.Endpoint, TotalCost: cost, ActualCost: cost, RateMultiplier: 1, BillingType: BillingTypeBalance,
		BillingMode: &mode, MediaType: &media, VideoCount: 1, VideoResolution: &t.Resolution, VideoDurationSeconds: &t.Duration,
		CreatedAt: t.CreatedAt}
	var ref falvideo.Reference
	if json.Unmarshal(t.Reference, &ref) == nil && ref.RequestID != "" {
		log.UpstreamRequestID = &ref.RequestID
	}
	_, err := s.usage.Create(ctx, log)
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) && sqlState.SQLState() == "23503" {
		// A hard-deleted account or key can prevent recording usage after capture.
		// Keep the durable task and committed settlement usable for reconciliation.
		logger.L().Warn("fal_video.usage_owner_deleted", zap.String("task_id", t.ID))
		return nil
	}
	return err
}

func (s *FalVideoService) invalidateBalance(ctx context.Context, userID int64) {
	if s.cache != nil {
		_ = s.cache.InvalidateUserBalance(ctx, userID)
	}
	if s.keys != nil {
		s.keys.InvalidateAuthCacheByUserID(ctx, userID)
	}
}
