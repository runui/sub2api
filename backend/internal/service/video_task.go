package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"go.uber.org/zap"
)

var (
	ErrVideoTaskInvalid                = errors.New("invalid video request")
	ErrVideoTaskNotFound               = errors.New("video request not found")
	ErrVideoTaskConflict               = errors.New("idempotency key already used for a different request")
	ErrVideoTaskSubmitting             = errors.New("video submission is still being recorded; check the request status")
	ErrVideoTaskUpstream               = errors.New("video upstream request failed")
	ErrVideoTaskSubmissionUnknown      = errors.New("video submission outcome unknown; do not resubmit automatically")
	ErrVideoTaskTemporarilyUnavailable = errors.New("video is temporarily unavailable")
	ErrVideoTaskInsufficientBalance    = errors.New("insufficient balance for video generation")
)

type VideoTaskRequest struct {
	UserID, APIKeyID, GroupID, SubscriptionID                        int64
	Model, Prompt, ImageURL, Resolution, AspectRatio, IdempotencyKey string
	RoutingModel                                                     string
	Duration                                                         int
	APIKey                                                           *APIKey
	Subscription                                                     *UserSubscription
}

type VideoTaskBilling interface {
	Reserve(context.Context, videotask.Task) error
	Settle(context.Context, videotask.Task, bool) error
}

type videoTaskInput struct {
	Prompt      string `json:"prompt"`
	ImageURL    string `json:"image_url,omitempty"`
	Duration    int    `json:"duration,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

type videoTaskReader interface {
	Get(context.Context, string) (videotask.Task, error)
	GetByIdempotencyKey(context.Context, int64, int64, int64, string) (videotask.Task, error)
}

type videoTaskExecutor interface {
	Submit(context.Context, VideoTaskRequest) (videotask.Task, error)
	OpenContent(context.Context, videotask.Task, string) (*http.Response, error)
}

// VideoTaskService owns the public task lifecycle. Providers handle their own upstream protocols.
type VideoTaskService struct {
	store     videoTaskReader
	executors map[string]videoTaskExecutor
}

func (s *VideoTaskService) SupportsProvider(provider string) bool {
	return s.executors[provider] != nil
}

func (s *VideoTaskService) Submit(ctx context.Context, provider string, req VideoTaskRequest) (videotask.Task, error) {
	executor := s.executors[provider]
	if executor == nil {
		return videotask.Task{}, ErrVideoTaskTemporarilyUnavailable
	}
	return executor.Submit(ctx, req)
}

func (s *VideoTaskService) Replay(ctx context.Context, req VideoTaskRequest) (videotask.Task, bool, error) {
	return replayVideoTask(ctx, s.store, req)
}

func (s *VideoTaskService) OpenContent(ctx context.Context, task videotask.Task, rangeHeader string) (*http.Response, error) {
	// A task keeps the original provider even after its model mapping changes.
	executor := s.executors[task.Provider]
	if executor == nil {
		return nil, ErrVideoTaskTemporarilyUnavailable
	}
	return executor.OpenContent(ctx, task, rangeHeader)
}

func (s *VideoTaskService) Get(ctx context.Context, id string, userID, keyID, groupID int64) (videotask.Task, error) {
	task, err := s.store.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (task.UserID != userID || task.APIKeyID != keyID || task.GroupID != groupID)) {
		return videotask.Task{}, ErrVideoTaskNotFound
	}
	if err != nil {
		return videotask.Task{}, ErrVideoTaskTemporarilyUnavailable
	}
	return task, nil
}

func validateVideoTaskInput(req VideoTaskRequest) (videoTaskInput, error) {
	input := videoTaskInput{Prompt: req.Prompt, ImageURL: req.ImageURL, Duration: req.Duration, Resolution: req.Resolution, AspectRatio: req.AspectRatio}
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 32<<10 || len(req.IdempotencyKey) > 128 || strings.ContainsAny(req.IdempotencyKey, "\r\n\x00") {
		return input, fmt.Errorf("%w: invalid prompt or Idempotency-Key", ErrVideoTaskInvalid)
	}
	if input.ImageURL != "" {
		u, err := url.Parse(input.ImageURL)
		if err != nil || len(input.ImageURL) > 8192 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
			return input, fmt.Errorf("%w: image must be an HTTPS URL", ErrVideoTaskInvalid)
		}
	}
	return input, nil
}

func replayVideoTask(ctx context.Context, store videoTaskReader, req VideoTaskRequest) (videotask.Task, bool, error) {
	var empty videotask.Task
	if req.IdempotencyKey == "" {
		return empty, false, nil
	}
	task, err := store.GetByIdempotencyKey(ctx, req.UserID, req.APIKeyID, req.GroupID, req.IdempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, ErrVideoTaskTemporarilyUnavailable
	}
	fingerprint, err := videoTaskRequestFingerprint(req)
	if err != nil || task.Fingerprint != fingerprint {
		return empty, false, ErrVideoTaskConflict
	}
	return task, true, videoTaskReplayError(task)
}

func videoTaskRequestFingerprint(req VideoTaskRequest) (string, error) {
	// Hash the original inputs, not defaults from a mutable profile.
	input, err := validateVideoTaskInput(req)
	return videoTaskFingerprint(req.Model, input), err
}

func videoTaskReplayError(task videotask.Task) error {
	if task.State == "submitting" {
		return ErrVideoTaskSubmitting
	}
	return nil
}

func videoTaskFingerprint(model string, input videoTaskInput) string {
	payload, _ := json.Marshal(input) // Input consists exclusively of strings and integers.
	// Idempotency describes the client input, not current routing or pricing.
	hash := sha256.Sum256(append([]byte(model+"\x00"), payload...))
	return "v1:" + hex.EncodeToString(hash[:])
}

func settleVideoTask(ctx context.Context, billing VideoTaskBilling, usage UsageLogRepository, cache *BillingCacheService, keys *APIKeyService, t videotask.Task, success bool, upstreamID string) error {
	if err := billing.Settle(ctx, t, success); err != nil {
		return err
	}
	invalidateVideoTaskBalance(ctx, cache, keys, t.UserID)
	if success && cache != nil {
		_ = cache.InvalidateAPIKeyRateLimit(ctx, t.APIKeyID)
	}
	if !success {
		return nil
	}
	cost := t.QuotedAmountUSD
	mode, media, path := string(BillingModeVideo), "video", "/v1/videos/generations"
	log := &UsageLog{UserID: t.UserID, APIKeyID: t.APIKeyID, AccountID: t.AccountID, GroupID: &t.GroupID,
		RequestID: t.ID, Model: t.Model, RequestedModel: t.Model, UpstreamModel: &t.Endpoint, InboundEndpoint: &path,
		UpstreamEndpoint: &t.Endpoint, TotalCost: cost, ActualCost: cost, RateMultiplier: 1, BillingType: BillingTypeBalance,
		BillingMode: &mode, MediaType: &media, VideoCount: 1, VideoResolution: &t.Resolution, VideoDurationSeconds: &t.Duration,
		CreatedAt: t.CreatedAt}
	if upstreamID != "" {
		log.UpstreamRequestID = &upstreamID
	}
	_, err := usage.Create(ctx, log)
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) && sqlState.SQLState() == "23503" {
		// A hard-deleted account or key can prevent recording usage after capture.
		// Keep the durable task and committed settlement usable for reconciliation.
		logger.L().Warn("video_task.usage_owner_deleted", zap.String("task_id", t.ID))
		return nil
	}
	return err
}

func invalidateVideoTaskBalance(ctx context.Context, cache *BillingCacheService, keys *APIKeyService, userID int64) {
	if cache != nil {
		_ = cache.InvalidateUserBalance(ctx, userID)
	}
	if keys != nil {
		keys.InvalidateAuthCacheByUserID(ctx, userID)
	}
}
