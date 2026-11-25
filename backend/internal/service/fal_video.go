package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"go.uber.org/zap"
)

const (
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

type falVideoStore interface {
	videoTaskReader
	Create(context.Context, videotask.Task) (videotask.Task, bool, error)
	Accept(context.Context, string, []byte) error
	Reject(context.Context, string, string, string) error
	Claim(context.Context, string, int, time.Duration) ([]videotask.Task, error)
	Reschedule(context.Context, string, string, string, time.Time) error
	Finish(context.Context, string, string, string, string, float64, int, int, string) error
	MarkStaleSubmissions(context.Context, string, time.Time) error
}

type FalVideoService struct {
	store       falVideoStore
	accounts    AccountRepository
	transport   HTTPUpstream
	contentDo   func(*http.Request) (*http.Response, error)
	billing     VideoTaskBilling
	usage       UsageLogRepository
	cache       *BillingCacheService
	keys        *APIKeyService
	concurrency *ConcurrencyService
	gateway     *GatewayService
	stop        context.CancelFunc
	worker      sync.WaitGroup
}

func ProvideFalVideoService(db *sql.DB, accounts AccountRepository, transport HTTPUpstream,
	billing VideoTaskBilling, usage UsageLogRepository, cache *BillingCacheService, keys *APIKeyService, concurrency *ConcurrencyService, gateway *GatewayService) *FalVideoService {
	s := &FalVideoService{store: videotask.NewStore(db), accounts: accounts,
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

func falVideoAccountModel(account *Account, model string) (string, bool) {
	if account == nil || account.Platform != PlatformFal || account.Type != AccountTypeAPIKey || !hasFalVideoAPIKey(account.Credentials) || !explicitModelMappingClaims(*account, model) {
		return "", false
	}
	upstream := strings.TrimSpace(account.GetMappedModel(model))
	return upstream, falVideoRegisteredModel(upstream)
}

func falVideoInput(req VideoTaskRequest) (falvideo.Input, error) {
	canonical, err := validateVideoTaskInput(req)
	input := falvideo.Input(canonical)
	if err != nil {
		return input, err
	}
	if input.Duration == 0 {
		input.Duration = 6
	}
	if input.Duration < 1 || input.Duration > 15 {
		return input, fmt.Errorf("%w: duration must be 1 to 15 seconds", ErrVideoTaskInvalid)
	}
	if input.Resolution == "" {
		input.Resolution = "720p"
	}
	if !slices.Contains(falImagineVideoResolutions, input.Resolution) {
		return input, fmt.Errorf("%w: unsupported resolution", ErrVideoTaskInvalid)
	}
	if input.ImageURL != "" {
		if input.AspectRatio != "" {
			return input, fmt.Errorf("%w: aspect_ratio is not supported with image input", ErrVideoTaskInvalid)
		}
	} else {
		switch input.AspectRatio {
		case "", "16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16", "21:9", "5:2":
		default:
			return input, fmt.Errorf("%w: unsupported aspect_ratio", ErrVideoTaskInvalid)
		}
		if input.AspectRatio == "" {
			input.AspectRatio = "16:9"
		}
	}
	return input, nil
}

func (s *FalVideoService) Submit(ctx context.Context, req VideoTaskRequest) (videotask.Task, error) {
	var empty videotask.Task
	if req.GroupID <= 0 || req.UserID <= 0 || req.APIKeyID <= 0 || req.APIKey == nil || req.APIKey.Group == nil || req.APIKey.Group.ID != req.GroupID {
		return empty, ErrVideoTaskNotFound
	}
	if replay, found, err := replayVideoTask(ctx, s.store, req); found || err != nil {
		return replay, err
	}
	if !req.APIKey.Group.AllowVideoGeneration {
		return empty, ErrVideoGenerationDisabled
	}
	if req.SubscriptionID != 0 || req.Subscription != nil || req.APIKey.Group.IsSubscriptionType() {
		return empty, fmt.Errorf("%w: fal video requires a balance-billed group", ErrVideoTaskInvalid)
	}
	routingModel := firstNonEmpty(req.RoutingModel, req.Model)
	group := req.APIKey.Group
	multiplier := group.RateMultiplier
	if s.gateway != nil && !group.VideoRateIndependent {
		multiplier = s.gateway.ResolveUserGroupRateMultiplier(ctx, req.UserID, req.GroupID, multiplier)
	}
	plan, release, err := s.selectAccount(ctx, req, routingModel, resolveVideoRateMultiplier(req.APIKey, multiplier))
	if err != nil {
		return empty, err
	}
	defer release()
	account, profile, input := plan.account, plan.profile, plan.input
	endpoint := profile.Endpoint
	fingerprint, err := videoTaskRequestFingerprint(req)
	if err != nil {
		return empty, err
	}
	task := videotask.Task{ID: videotask.NewID(), Provider: PlatformFal, Model: req.Model, Endpoint: endpoint, Resolution: input.Resolution,
		UserID: req.UserID, APIKeyID: req.APIKeyID, GroupID: req.GroupID, AccountID: account.ID,
		Duration: input.Duration, QuotedAmountUSD: plan.amount, IdempotencyKey: req.IdempotencyKey, Fingerprint: fingerprint,
		ProfileVersion: profile.Version, ResultPath: strings.Join(profile.ResultPath, ".")}
	task, created, err := s.store.Create(ctx, task)
	if err != nil {
		return empty, ErrVideoTaskTemporarilyUnavailable
	}
	// Only the request that creates the task may reserve funds and POST to fal.
	if !created {
		original, err := videoTaskRequestFingerprint(req)
		if err != nil || task.Fingerprint != original {
			return empty, ErrVideoTaskConflict
		}
		return task, videoTaskReplayError(task)
	}

	// A disconnected downstream must not prevent recording the POST outcome.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*falVideoHTTPTimeout)
	defer cancel()
	if err := s.billing.Reserve(work, task); err != nil {
		s.reject(work, task.ID, "reservation_failed")
		if errors.Is(err, ErrBatchImageInsufficientBalance) {
			return task, ErrVideoTaskInsufficientBalance
		}
		if errors.Is(err, ErrVideoTaskKeyLimit) {
			return task, ErrVideoTaskKeyLimit
		}
		return task, ErrVideoTaskTemporarilyUnavailable
	}
	invalidateVideoTaskBalance(work, s.cache, s.keys, task.UserID)
	requestCtx, stop := context.WithTimeout(work, falVideoHTTPTimeout)
	client, key := s.client(account), account.GetCredential("api_key")
	ref, err := client.SubmitJSON(requestCtx, key, endpoint, plan.payload)
	stop()
	if err != nil {
		var upstream *falvideo.Error
		if errors.As(err, &upstream) && upstream.StatusCode >= 400 && upstream.StatusCode < 500 && upstream.StatusCode != http.StatusRequestTimeout {
			s.reject(work, task.ID, "upstream_rejected")
			if upstream.StatusCode == http.StatusUnprocessableEntity || upstream.StatusCode == http.StatusBadRequest {
				return task, fmt.Errorf("%w: fal rejected the input", ErrVideoTaskInvalid)
			}
			return task, ErrVideoTaskUpstream
		}
		// A failed response does not prove the upstream did not enqueue the job.
		s.reject(work, task.ID, "submission_unknown")
		return task, ErrVideoTaskSubmissionUnknown
	}
	refs, err := json.Marshal(ref)
	if err != nil || s.store.Accept(work, task.ID, refs) != nil {
		logger.L().Error("fal_video.accept_persist_failed", zap.String("task_id", task.ID), zap.String("fal_request_id", ref.RequestID))
		return task, ErrVideoTaskSubmissionUnknown
	}
	task.State = "queued"
	return task, nil
}

// A selected account, wire payload, and quote are one decision. Repricing after
// account selection could reserve an amount for a different upstream contract.
type falVideoSubmission struct {
	account *Account
	profile falVideoProfile
	input   falvideo.Input
	payload json.RawMessage
	amount  float64
}

func (s *FalVideoService) selectAccount(ctx context.Context, req VideoTaskRequest, model string, multiplier float64) (falVideoSubmission, func(), error) {
	accounts, err := s.accounts.ListSchedulableByGroupIDAndPlatform(ctx, req.GroupID, PlatformFal)
	if err != nil {
		return falVideoSubmission{}, nil, ErrVideoTaskTemporarilyUnavailable
	}
	var invalid error
	var compatible bool
	// The repository orders by group priority, then account priority. Only
	// fall through to another account before any task or upstream side effect.
	for i := range accounts {
		account := &accounts[i]
		upstream, ok := falVideoAccountModel(account, model)
		if !ok || !account.IsSchedulable() {
			continue
		}
		// Resolve the request and price from the same configured endpoint.
		profile, input, payload, err := prepareFalVideo(upstream, req)
		if err != nil {
			invalid = err
			continue
		}
		compatible = true
		release := func() {}
		if s.concurrency != nil {
			slot, err := s.concurrency.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
			if err != nil {
				return falVideoSubmission{}, nil, ErrVideoTaskTemporarilyUnavailable
			}
			if slot == nil || !slot.Acquired {
				continue
			}
			release = slot.ReleaseFunc
		}
		// Quote only the selected account's contract: missing pricing must not reroute.
		amount, err := quoteFalVideo(req.APIKey.Group, profile, input, multiplier)
		if err != nil {
			release()
			return falVideoSubmission{}, nil, err
		}
		return falVideoSubmission{account: account, profile: profile, input: input, payload: payload, amount: amount}, release, nil
	}
	if invalid != nil && !compatible {
		return falVideoSubmission{}, nil, invalid
	}
	return falVideoSubmission{}, nil, ErrVideoTaskTemporarilyUnavailable
}

func (s *FalVideoService) reject(ctx context.Context, id, code string) {
	if err := s.store.Reject(ctx, id, "pending_failure", code); err != nil {
		logger.L().Warn("fal_video.reject_persist_failed", zap.String("task_id", id), zap.String("code", code))
	}
}

func (s *FalVideoService) account(ctx context.Context, id int64) (*Account, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Platform != PlatformFal || a.Type != AccountTypeAPIKey || !hasFalVideoAPIKey(a.Credentials) {
		return nil, ErrVideoTaskTemporarilyUnavailable
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

func (s *FalVideoService) OpenContent(ctx context.Context, task videotask.Task, rangeHeader string) (*http.Response, error) {
	if task.State != "succeeded" {
		return nil, ErrVideoTaskConflict
	}
	u, ok := falVideoCDN(task.ResultURL)
	if !ok {
		return nil, ErrVideoTaskUpstream
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(ctx)), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrVideoTaskUpstream
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
		return nil, ErrVideoTaskUpstream
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		if resp.ContentLength > falVideoMaxContentBytes {
			_ = resp.Body.Close()
			return nil, ErrVideoTaskUpstream
		}
		resp.Body = http.MaxBytesReader(nil, resp.Body, falVideoMaxContentBytes)
		return resp, nil
	case http.StatusRequestedRangeNotSatisfiable:
		return resp, nil
	case http.StatusNotFound, http.StatusGone:
		_ = resp.Body.Close()
		return nil, ErrVideoTaskNotFound
	default:
		_ = resp.Body.Close()
		return nil, ErrVideoTaskUpstream
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
	if err := s.store.MarkStaleSubmissions(dbCtx, PlatformFal, time.Now().Add(-2*time.Minute)); err != nil {
		cancel()
		return err
	}
	tasks, err := s.store.Claim(dbCtx, PlatformFal, 8, 2*time.Minute)
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

func (s *FalVideoService) advance(ctx context.Context, t videotask.Task) error {
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
		raw, err := client.ResultJSON(call, key, ref)
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
		// Parse with the job's original contract even if the profile was removed.
		result, err := falVideoResultAt(raw, strings.Split(t.ResultPath, "."))
		if _, ok := falVideoCDN(result.URL); err != nil || !ok {
			return finish("pending_failure", "invalid_video_output", falvideo.Result{})
		}
		if result.Duration == 0 {
			result.Duration = float64(t.Duration)
		}
		return finish("pending_success", "", result)
	default:
		return reschedule(t.State, 15*time.Second)
	}
}

func (s *FalVideoService) settle(ctx context.Context, t videotask.Task, success bool) error {
	var ref falvideo.Reference
	_ = json.Unmarshal(t.Reference, &ref)
	return settleVideoTask(ctx, s.billing, s.usage, s.cache, s.keys, t, success, ref.RequestID)
}
