package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type videoTasks interface {
	SupportsProvider(string) bool
	Replay(context.Context, service.VideoTaskRequest) (videotask.Task, bool, error)
	Submit(context.Context, string, service.VideoTaskRequest) (videotask.Task, error)
	Get(ctx context.Context, id string, userID, keyID, groupID int64) (videotask.Task, error)
	OpenContent(context.Context, videotask.Task, string) (*http.Response, error)
}

type VideoTaskHandler struct {
	tasks   videoTasks
	gateway *OpenAIGatewayHandler
}

func NewVideoTaskHandler(svc *service.VideoTaskService, gateway *OpenAIGatewayHandler) *VideoTaskHandler {
	h := &VideoTaskHandler{gateway: gateway}
	if svc != nil {
		h.tasks = svc
	}
	return h
}

func (h *VideoTaskHandler) SupportsProvider(provider string) bool {
	return h != nil && h.tasks != nil && h.tasks.SupportsProvider(provider)
}

// ReplayCreate is checked before resolving a new route or applying spending denial.
func (h *VideoTaskHandler) ReplayCreate(c *gin.Context, model string, body []byte) bool {
	if h == nil || h.tasks == nil || model == "" {
		return false
	}
	key, _ := middleware.GetAPIKeyFromContext(c)
	if key == nil || key.Group == nil || key.GroupID == nil || *key.GroupID <= 0 {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return h.tryReplay(c, key, model, mediaType, body)
}

func (h *VideoTaskHandler) Create(c *gin.Context, provider, model, routingModel string, body []byte) {
	if h == nil || h.tasks == nil {
		writeVideoTaskServiceError(c, service.ErrVideoTaskTemporarilyUnavailable, "")
		return
	}
	if key, subject, ok := videoTaskIdentity(c); ok {
		mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
		h.create(c, key, subject, provider, model, routingModel, mediaType, body)
	}
}

func (h *VideoTaskHandler) tryReplay(c *gin.Context, key *service.APIKey, model, mediaType string, body []byte) bool {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.HasPrefix(mediaType, "multipart/") ||
		key.Group.ID != *key.GroupID || key.Status == service.StatusAPIKeyExpired || key.IsExpired() {
		return false
	}
	req, err := parseVideoTaskRequest(body)
	if err != nil {
		return false
	}
	_, subject, ok := videoTaskIdentity(c)
	if !ok {
		return true
	}
	req.Model, req.UserID, req.APIKeyID, req.GroupID = model, subject.UserID, key.ID, *key.GroupID
	req.IdempotencyKey = strings.TrimSpace(values[0])
	task, found, err := h.tasks.Replay(c.Request.Context(), req)
	if !found && err == nil {
		return false
	}
	setOpsRequestContext(c, model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))
	if err != nil {
		writeVideoTaskServiceError(c, err, task.ID)
	} else {
		writeVideoTaskAcknowledgment(c, task.ID)
	}
	return true
}

func (h *VideoTaskHandler) create(c *gin.Context, key *service.APIKey, subject middleware.AuthSubject, provider, model, routingModel, mediaType string, body []byte) {
	if key.Status == service.StatusAPIKeyExpired || key.IsExpired() {
		videoTaskError(c, http.StatusForbidden, "authentication_error", "API_KEY_EXPIRED", "API key has expired", "")
		return
	}
	if key.Group == nil || key.Group.ID != *key.GroupID {
		writeVideoTaskServiceError(c, service.ErrVideoTaskNotFound, "")
		return
	}
	if !key.Group.AllowVideoGeneration {
		writeVideoTaskServiceError(c, service.ErrVideoGenerationDisabled, "")
		return
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", "Send a JSON video generation request", "")
		return
	}
	req, err := parseVideoTaskRequest(body)
	if err != nil {
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", err.Error(), "")
		return
	}
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	if subscription != nil || key.Group.IsSubscriptionType() {
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", "Video generation requires a balance-billed group", "")
		return
	}
	req.Model, req.UserID, req.APIKeyID, req.GroupID = model, subject.UserID, key.ID, *key.GroupID
	req.APIKey, req.RoutingModel = key, routingModel
	if values := c.Request.Header.Values("Idempotency-Key"); len(values) > 1 {
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", "Send only one Idempotency-Key header", "")
		return
	}
	req.IdempotencyKey = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	setOpsRequestContext(c, model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))
	if h.gateway != nil {
		reqLog := requestLogger(c, "handler.video_task.create", zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", key.ID), zap.String("model", model))
		moderation := service.OpenAIImagesRequest{Prompt: req.Prompt}
		if req.ImageURL != "" {
			moderation.InputImageURLs = []string{req.ImageURL}
		}
		decision := h.gateway.checkSecurityAudit(c, reqLog, key, subject, service.ContentModerationProtocolOpenAIImages, model, moderation.ModerationBody())
		if decision != nil && !decision.AllowNextStage {
			h.gateway.openAISecurityAuditError(c, decision)
			return
		}
		billing := h.gateway.billingCacheService
		if billing == nil || key.User == nil || h.gateway.concurrencyHelper == nil {
			writeVideoTaskServiceError(c, service.ErrVideoTaskTemporarilyUnavailable, "")
			return
		}
		streamStarted := false
		mediaRelease, acquired := h.gateway.acquireImageGenerationSlot(c, streamStarted)
		if !acquired {
			return
		}
		if mediaRelease != nil {
			defer mediaRelease()
		}
		release, acquired := h.gateway.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
		if !acquired {
			return
		}
		if release != nil {
			defer release()
		}
		ctx := c.Request.Context()
		if err := billing.CheckBillingEligibility(ctx, key.User, key, key.Group, subscription, service.QuotaPlatform(ctx, key)); err != nil {
			status, code, message, retryAfter := billingErrorDetails(err)
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			h.gateway.errorResponse(c, status, code, message)
			return
		}
	}

	task, err := h.tasks.Submit(c.Request.Context(), provider, req)
	if err != nil {
		writeVideoTaskServiceError(c, err, task.ID)
		return
	}
	writeVideoTaskAcknowledgment(c, task.ID)
}

func writeVideoTaskAcknowledgment(c *gin.Context, id string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"request_id": id})
}

func parseVideoTaskRequest(body []byte) (service.VideoTaskRequest, error) {
	var req service.VideoTaskRequest
	var fields map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		return req, errors.New("Send a valid UTF-8 JSON object")
	}
	if _, conflict := requestmodel.ConflictingField("application/json", body); conflict {
		return req, errors.New("Request body contains duplicate fields")
	}
	var image json.RawMessage
	for field, value := range fields {
		var dest any
		switch field {
		case "model":
			dest = &req.Model
		case "prompt":
			dest = &req.Prompt
		case "duration":
			dest = &req.Duration
		case "resolution":
			dest = &req.Resolution
		case "aspect_ratio":
			dest = &req.AspectRatio
		case "image", "image_url":
			if image != nil {
				return req, errors.New("Specify only one of image and image_url")
			}
			image = value
			continue
		default:
			return req, fmt.Errorf("Unsupported video generation field %.64q", field)
		}
		if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, dest) != nil {
			return req, fmt.Errorf("Invalid value for %s", field)
		}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return req, errors.New("prompt is required")
	}
	if _, specified := fields["duration"]; specified && req.Duration <= 0 {
		return req, errors.New("duration must be a positive integer")
	}
	if image != nil {
		rawURL := strings.TrimSpace(videoTaskImageURL(image))
		u, err := url.Parse(rawURL)
		if err != nil || len(rawURL) > 8192 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			strings.ContainsAny(rawURL, " \\#") || (u.Port() != "" && u.Port() != "443") ||
			urlvalidator.IsBlockedHost(strings.TrimSuffix(u.Hostname(), ".")) {
			return req, errors.New("image must contain a single public HTTPS URL")
		}
		req.ImageURL = rawURL
	}
	return req, nil
}

func videoTaskImageURL(raw json.RawMessage) string {
	var imageURL string
	if json.Unmarshal(raw, &imageURL) == nil {
		return imageURL
	}
	var image map[string]json.RawMessage
	if json.Unmarshal(raw, &image) != nil {
		return ""
	}
	for key, value := range image {
		switch key {
		case "url", "image_url":
		case "type":
			var imageType string
			if json.Unmarshal(value, &imageType) != nil || imageType != "image_url" {
				return ""
			}
		default:
			return ""
		}
	}
	if direct, ok := image["url"]; ok {
		if _, conflict := image["image_url"]; conflict || json.Unmarshal(direct, &imageURL) != nil {
			return ""
		}
		return imageURL
	}
	legacy := image["image_url"]
	if json.Unmarshal(legacy, &imageURL) == nil {
		return imageURL
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(legacy, &nested) == nil && len(nested) == 1 && json.Unmarshal(nested["url"], &imageURL) == nil {
		return imageURL
	}
	return ""
}

func videoTaskIdentity(c *gin.Context) (*service.APIKey, middleware.AuthSubject, bool) {
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, found := middleware.GetAuthSubjectFromContext(c)
	if key == nil || key.ID <= 0 || !found || subject.UserID <= 0 || (key.UserID > 0 && key.UserID != subject.UserID) {
		videoTaskError(c, http.StatusUnauthorized, "authentication_error", "", "Invalid API key", "")
		return nil, subject, false
	}
	if key.GroupID == nil || *key.GroupID <= 0 {
		writeVideoTaskServiceError(c, service.ErrVideoTaskNotFound, "")
		return nil, subject, false
	}
	return key, subject, true
}

func (h *VideoTaskHandler) lookup(c *gin.Context) (videotask.Task, bool) {
	key, subject, ok := videoTaskIdentity(c)
	if !ok {
		return videotask.Task{}, false
	}
	id := strings.TrimSpace(c.Param("request_id"))
	if id == "" {
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", "request_id is required", "")
		return videotask.Task{}, false
	}
	if !videotask.IsID(id) {
		writeVideoTaskServiceError(c, service.ErrVideoTaskNotFound, "")
		return videotask.Task{}, false
	}
	if h == nil || h.tasks == nil {
		writeVideoTaskServiceError(c, service.ErrVideoTaskTemporarilyUnavailable, "")
		return videotask.Task{}, false
	}
	task, err := h.tasks.Get(c.Request.Context(), id, subject.UserID, key.ID, *key.GroupID)
	if err != nil {
		writeVideoTaskServiceError(c, err, "")
		return videotask.Task{}, false
	}
	setOpsRequestContext(c, task.Model, false)
	return task, true
}

func (h *VideoTaskHandler) Status(c *gin.Context) {
	task, ok := h.lookup(c)
	if !ok {
		return
	}
	response := gin.H{"request_id": task.ID, "model": task.Model, "status": "pending"}
	switch task.State {
	case "succeeded":
		prefix := ""
		if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			prefix = "/v1"
		}
		duration := task.ActualDuration
		if duration <= 0 {
			duration = float64(task.Duration)
		}
		video := gin.H{"url": prefix + "/videos/" + url.PathEscape(task.ID) + "/content", "duration": duration}
		if task.Width > 0 {
			video["width"] = task.Width
		}
		if task.Height > 0 {
			video["height"] = task.Height
		}
		if task.Resolution != "" {
			video["resolution"] = task.Resolution
		}
		response["status"], response["video"] = "done", video
	case "failed":
		response["status"] = "failed"
		response["error"] = gin.H{"code": firstNonEmptyString(task.ErrorCode, "generation_failed"), "message": "Video generation failed"}
	case "unknown":
		response["status"] = "failed"
		response["error"] = gin.H{"code": "submission_unknown", "message": service.ErrVideoTaskSubmissionUnknown.Error()}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, response)
}

func (h *VideoTaskHandler) Content(c *gin.Context) {
	task, ok := h.lookup(c)
	if !ok {
		return
	}
	if task.State != "succeeded" {
		videoTaskError(c, http.StatusConflict, "invalid_request_error", "video_not_ready", "Video content is not available", "")
		return
	}
	resp, err := h.tasks.OpenContent(c.Request.Context(), task, strings.TrimSpace(c.GetHeader("Range")))
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		writeVideoTaskServiceError(c, err, "")
		return
	}
	if resp == nil || resp.Body == nil {
		writeVideoTaskServiceError(c, service.ErrVideoTaskUpstream, "")
		return
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		copyVideoTaskRangeHeaders(c, resp)
		videoTaskError(c, resp.StatusCode, "invalid_request_error", "range_not_satisfiable", "Requested video range is not satisfiable", "")
		return
	}
	contentType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.Header.Get("Content-Type") == "" {
		contentType, err = "application/octet-stream", nil
	}
	if (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent) || err != nil ||
		!(strings.HasPrefix(contentType, "video/") || contentType == "application/octet-stream" ||
			(resp.StatusCode == http.StatusPartialContent && contentType == "multipart/byteranges" && params["boundary"] != "")) {
		writeVideoTaskServiceError(c, service.ErrVideoTaskUpstream, "")
		return
	}

	copyVideoTaskRangeHeaders(c, resp)
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Type", mime.FormatMediaType(contentType, params))
	if resp.ContentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	c.Status(resp.StatusCode)
	service.MarkResponseCommitted(c)
	c.Writer.WriteHeaderNow()
	if _, err := io.Copy(c.Writer, resp.Body); err != nil && c.Request.Context().Err() == nil {
		requestLogger(c, "handler.video_task.content").Warn("video_task.content_stream_failed", zap.String("request_id", task.ID))
	}
}

func copyVideoTaskRangeHeaders(c *gin.Context, resp *http.Response) {
	for _, name := range []string{"Accept-Ranges", "Content-Range"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
}

func writeVideoTaskServiceError(c *gin.Context, err error, id string) {
	switch {
	case errors.Is(err, service.ErrVideoGenerationDisabled):
		videoTaskError(c, http.StatusForbidden, "permission_error", "video_generation_disabled", service.ErrVideoGenerationDisabled.Error(), "")
	case errors.Is(err, service.ErrVideoModelPriceMissing):
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "video_price_missing", service.ErrVideoModelPriceMissing.Error(), "")
	case errors.Is(err, service.ErrVideoTaskInvalid):
		videoTaskError(c, http.StatusBadRequest, "invalid_request_error", "", service.ErrVideoTaskInvalid.Error(), "")
	case errors.Is(err, service.ErrVideoTaskNotFound):
		videoTaskError(c, http.StatusNotFound, "not_found_error", "", "Video request not found", "")
	case errors.Is(err, service.ErrVideoTaskConflict):
		videoTaskError(c, http.StatusConflict, "invalid_request_error", "idempotency_conflict", service.ErrVideoTaskConflict.Error(), "")
	case errors.Is(err, service.ErrVideoTaskSubmitting):
		videoTaskError(c, http.StatusConflict, "api_error", "submission_in_progress", service.ErrVideoTaskSubmitting.Error(), id)
	case errors.Is(err, service.ErrVideoTaskInsufficientBalance):
		videoTaskError(c, http.StatusForbidden, "billing_error", "insufficient_balance", service.ErrVideoTaskInsufficientBalance.Error(), "")
	case errors.Is(err, service.ErrVideoTaskKeyLimit):
		videoTaskError(c, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded", service.ErrVideoTaskKeyLimit.Error(), "")
	case errors.Is(err, service.ErrVideoTaskSubmissionUnknown):
		videoTaskError(c, http.StatusBadGateway, "upstream_error", "submission_unknown", service.ErrVideoTaskSubmissionUnknown.Error(), id)
	case errors.Is(err, service.ErrVideoTaskUpstream):
		videoTaskError(c, http.StatusBadGateway, "upstream_error", "", "Video provider request failed", "")
	case errors.Is(err, service.ErrVideoTaskTemporarilyUnavailable):
		videoTaskError(c, http.StatusServiceUnavailable, "api_error", "", "Video generation is temporarily unavailable", "")
	default:
		videoTaskError(c, http.StatusInternalServerError, "api_error", "", "Video request failed", "")
	}
}

func videoTaskError(c *gin.Context, status int, errorType, code, message, id string) {
	payload := gin.H{"type": errorType, "message": message}
	if code != "" {
		payload["code"] = code
	}
	response := gin.H{"error": payload}
	if id != "" {
		response["request_id"] = id
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, response)
}
