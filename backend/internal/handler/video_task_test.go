package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const videoTestModel = "test-video-model"

type videoTaskTestTasks struct {
	submit func(service.VideoTaskRequest) (videotask.Task, error)
	replay func(service.VideoTaskRequest) (videotask.Task, bool, error)
	get    func(string, int64, int64, int64) (videotask.Task, error)
	open   func(videotask.Task, string) (*http.Response, error)
}

func (*videoTaskTestTasks) SupportsProvider(string) bool { return true }

func (s *videoTaskTestTasks) Replay(_ context.Context, req service.VideoTaskRequest) (videotask.Task, bool, error) {
	if s.replay == nil {
		return videotask.Task{}, false, nil
	}
	return s.replay(req)
}

func TestVideoTaskReplayBeforeBillingWithRouteRemoved(t *testing.T) {
	h := &VideoTaskHandler{gateway: &OpenAIGatewayHandler{}, tasks: &videoTaskTestTasks{
		replay: func(req service.VideoTaskRequest) (videotask.Task, bool, error) {
			require.Equal(t, "original", req.IdempotencyKey)
			return videotask.Task{ID: "vidtask_123"}, true, nil
		},
	}}
	c, w, _ := videoTaskTestContext(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"`+videoTestModel+`","prompt":"waves"}`))
	c.Request.Header.Set("Idempotency-Key", "original")
	body, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	require.True(t, h.ReplayCreate(c, videoTestModel, body))
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"request_id":"vidtask_123"}`, w.Body.String())
}

func (s *videoTaskTestTasks) Submit(_ context.Context, _ string, req service.VideoTaskRequest) (videotask.Task, error) {
	return s.submit(req)
}

func (s *videoTaskTestTasks) Get(_ context.Context, id string, userID, keyID, groupID int64) (videotask.Task, error) {
	return s.get(id, userID, keyID, groupID)
}

func (s *videoTaskTestTasks) OpenContent(_ context.Context, task videotask.Task, requestedRange string) (*http.Response, error) {
	return s.open(task, requestedRange)
}

func videoTaskTestContext(method, path string, body io.Reader) (*gin.Context, *httptest.ResponseRecorder, *service.APIKey) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, body)
	c.Params = gin.Params{{Key: "request_id", Value: "vidtask_123"}}
	groupID := int64(9)
	key := &service.APIKey{ID: 8, UserID: 7, GroupID: &groupID, Group: &service.Group{ID: groupID, AllowImageGeneration: true}}
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	c.Set(string(middleware.ContextKeyAPIKey), key)
	return c, w, key
}

func TestVideoTaskCreate(t *testing.T) {
	c, w, key := videoTaskTestContext(http.MethodPost, "/v1/videos", strings.NewReader(`{
		"model":"`+videoTestModel+`","prompt":"Make waves","duration":5,"resolution":"720p",
		"image":{"type":"image_url","image_url":{"url":"https://img.example.com/a%2Fb/?signed=1"}}
	}`))
	c.Request.Header.Set("Idempotency-Key", "client-key")
	h := &VideoTaskHandler{tasks: &videoTaskTestTasks{submit: func(req service.VideoTaskRequest) (videotask.Task, error) {
		require.Equal(t, service.VideoTaskRequest{
			UserID: 7, APIKeyID: 8, GroupID: 9, APIKey: key,
			Model: videoTestModel, RoutingModel: videoTestModel, Prompt: "Make waves", Duration: 5, Resolution: "720p", IdempotencyKey: "client-key",
			ImageURL: "https://img.example.com/a%2Fb/?signed=1",
		}, req)
		return videotask.Task{ID: "vidtask_123"}, nil
	}}}
	createVideoTaskForTest(t, h, c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"request_id":"vidtask_123"}`, w.Body.String())
}

func TestVideoTaskCreateRejectsUnsupportedFields(t *testing.T) {
	h := &VideoTaskHandler{tasks: &videoTaskTestTasks{}}
	for _, fields := range []string{
		`"extra":1`, `"Duration":5`, `"duration":"5"`, `"duration":0`, `"duration":null`,
		`"duration":5,"duration":6`, `"image":["https://example.com/a"]`, `"image":null`,
		`"image":{"url":7}`, `"image":{"url":"https://example.com/a","image_url":"https://example.com/b"}`,
		`"image":"https://example.com/a","image_url":"https://example.com/b"`,
		`"image":{"image_url":{"url":"https://example.com/a","other":true}}`,
		`"image":{"url":"https://example.com/a","type":"other"}`,
		`"image":"http://example.com/a"`, `"image":"data:image/png;base64,aaaa"`,
		`"image":"https://user@example.com/a"`, `"image":"https://example.com/a#"`,
		`"image":"https://example.com:123/a"`, `"image":"https://localhost./a"`,
	} {
		t.Run(fields, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":%q,"prompt":"waves",%s}`, videoTestModel, fields)
			c, w, _ := videoTaskTestContext(http.MethodPost, "/videos/generations", strings.NewReader(body))
			createVideoTaskForTest(t, h, c)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), `"type":"invalid_request_error"`)
		})
	}
}

func TestVideoTaskCreatePermissionsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"disabled group", nil, http.StatusForbidden},
		{"unavailable", service.ErrVideoTaskTemporarilyUnavailable, http.StatusServiceUnavailable},
		{"idempotency conflict", service.ErrVideoTaskConflict, http.StatusConflict},
		{"unknown submission", service.ErrVideoTaskSubmissionUnknown, http.StatusBadGateway},
		{"insufficient balance", service.ErrVideoTaskInsufficientBalance, http.StatusForbidden},
		{"key spending limit", service.ErrVideoTaskKeyLimit, http.StatusTooManyRequests},
		{"provider failure", service.ErrVideoTaskUpstream, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w, key := videoTaskTestContext(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"`+videoTestModel+`","prompt":"waves"}`))
			key.Group.AllowImageGeneration = tc.err != nil
			h := &VideoTaskHandler{tasks: &videoTaskTestTasks{submit: func(service.VideoTaskRequest) (videotask.Task, error) {
				require.NotNil(t, tc.err, "disabled groups must not submit")
				return videotask.Task{ID: "vidtask_123"}, fmt.Errorf("provider debug secret: %w", tc.err)
			}}}
			createVideoTaskForTest(t, h, c)
			require.Equal(t, tc.want, w.Code)
			require.NotContains(t, w.Body.String(), "provider debug secret")
			if tc.err == service.ErrVideoTaskSubmissionUnknown {
				require.Contains(t, w.Body.String(), `"request_id":"vidtask_123"`)
			}
		})
	}
}

func TestVideoTaskCreateReleasesGatewayUserSlot(t *testing.T) {
	for _, submitErr := range []error{nil, service.ErrVideoTaskUpstream} {
		c, w, key := videoTaskTestContext(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+videoTestModel+`","prompt":"waves"}`))
		key.User = &service.User{ID: key.UserID}
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 1})
		cache := &helperConcurrencyCacheStub{userSeq: []bool{true}}
		billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil)
		t.Cleanup(billing.Stop)
		h := &VideoTaskHandler{
			gateway: &OpenAIGatewayHandler{
				billingCacheService: billing,
				concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0),
			},
			tasks: &videoTaskTestTasks{submit: func(service.VideoTaskRequest) (videotask.Task, error) {
				require.Equal(t, 1, cache.userAcquireCalls)
				require.Zero(t, cache.userReleaseCalls)
				return videotask.Task{ID: "vidtask_123"}, submitErr
			}},
		}
		createVideoTaskForTest(t, h, c)
		require.Equal(t, 1, cache.userReleaseCalls)
		if submitErr == nil {
			require.Equal(t, http.StatusOK, w.Code)
		} else {
			require.Equal(t, http.StatusBadGateway, w.Code)
		}
	}
}

func TestVideoTaskCreateRequiresBalanceGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*gin.Context, *service.APIKey)
		want  int
	}{
		{"mismatched group", func(_ *gin.Context, key *service.APIKey) { key.Group.ID++ }, http.StatusNotFound},
		{"subscription context", func(c *gin.Context, _ *service.APIKey) {
			c.Set(string(middleware.ContextKeySubscription), &service.UserSubscription{ID: 21})
		}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w, key := videoTaskTestContext(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+videoTestModel+`","prompt":"waves"}`))
			tc.apply(c, key)
			h := &VideoTaskHandler{tasks: &videoTaskTestTasks{}}
			createVideoTaskForTest(t, h, c)
			require.Equal(t, tc.want, w.Code)
		})
	}
}

func TestVideoTaskStatus(t *testing.T) {
	for _, tc := range []struct{ state, status string }{
		{"submitting", "pending"}, {"queued", "pending"}, {"running", "pending"}, {"pending_success", "pending"},
		{"pending_failure", "pending"}, {"succeeded", "done"}, {"failed", "failed"}, {"unknown", "failed"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			for _, prefix := range []string{"", "/v1"} {
				c, w, _ := videoTaskTestContext(http.MethodGet, prefix+"/videos/generations/vidtask_123", nil)
				h := &VideoTaskHandler{tasks: &videoTaskTestTasks{get: func(id string, user, key, group int64) (videotask.Task, error) {
					require.Equal(t, "vidtask_123", id)
					require.Equal(t, []int64{7, 8, 9}, []int64{user, key, group})
					return videotask.Task{ID: id, Model: videoTestModel, State: tc.state, ResultURL: "https://cdn.example.test/secret",
						ActualDuration: 4.5, Duration: 5, Width: 1280, Height: 720, Resolution: "720p"}, nil
				}}}
				h.Status(c)
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), `"status":"`+tc.status+`"`)
				require.NotContains(t, w.Body.String(), "cdn.example.test")
				if tc.state == "succeeded" {
					require.JSONEq(t, fmt.Sprintf(`{"request_id":"vidtask_123","status":"done","model":%q,"video":{
						"url":%q,"duration":4.5,"width":1280,"height":720,"resolution":"720p"}}`,
						videoTestModel, prefix+"/videos/vidtask_123/content"), w.Body.String())
				} else {
					require.NotContains(t, w.Body.String(), `"video"`)
				}
			}
		})
	}
}

func TestVideoTaskStatusRejectsWrongOwner(t *testing.T) {
	c, w, _ := videoTaskTestContext(http.MethodGet, "/v1/videos/vidtask_123", nil)
	h := &VideoTaskHandler{tasks: &videoTaskTestTasks{get: func(string, int64, int64, int64) (videotask.Task, error) {
		return videotask.Task{}, fmt.Errorf("private lookup reason: %w", service.ErrVideoTaskNotFound)
	}}}
	h.Status(c)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.NotContains(t, w.Body.String(), "private lookup reason")
}

func TestVideoTaskLookupRejectsNonTaskIDAndUnauthenticatedRequests(t *testing.T) {
	h := &VideoTaskHandler{tasks: &videoTaskTestTasks{}}
	c, w, _ := videoTaskTestContext(http.MethodGet, "/v1/videos/legacy", nil)
	c.Params[0].Value = "legacy"
	h.Status(c)
	require.Equal(t, http.StatusNotFound, w.Code)

	c, w, _ = videoTaskTestContext(http.MethodGet, "/v1/videos/vidtask_123/content", nil)
	c.Set(string(middleware.ContextKeyAPIKey), nil)
	h.Content(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

type videoTaskTestBody struct {
	io.Reader
	closed bool
}

func (b *videoTaskTestBody) Close() error {
	b.closed = true
	return nil
}

func TestVideoTaskContentRange(t *testing.T) {
	for _, tc := range []struct {
		status      int
		contentType string
		want        int
	}{
		{http.StatusOK, "video/mp4", http.StatusOK},
		{http.StatusPartialContent, "video/mp4", http.StatusPartialContent},
		{http.StatusRequestedRangeNotSatisfiable, "text/html", http.StatusRequestedRangeNotSatisfiable},
		{http.StatusOK, "text/html", http.StatusBadGateway},
		{http.StatusFound, "video/mp4", http.StatusBadGateway},
	} {
		t.Run(fmt.Sprintf("%d %s", tc.status, tc.contentType), func(t *testing.T) {
			c, w, _ := videoTaskTestContext(http.MethodGet, "/v1/videos/vidtask_123/content", nil)
			c.Request.Header.Set("Range", "bytes=0-2")
			body := &videoTaskTestBody{Reader: strings.NewReader("mp4")}
			h := &VideoTaskHandler{tasks: &videoTaskTestTasks{
				get: func(id string, user, key, group int64) (videotask.Task, error) {
					require.Equal(t, []int64{7, 8, 9}, []int64{user, key, group})
					return videotask.Task{ID: id, State: "succeeded"}, nil
				},
				open: func(task videotask.Task, requestedRange string) (*http.Response, error) {
					require.Equal(t, "vidtask_123", task.ID)
					require.Equal(t, "bytes=0-2", requestedRange)
					header := http.Header{}
					header.Set("Content-Type", tc.contentType)
					header.Set("Accept-Ranges", "bytes")
					if tc.status == http.StatusRequestedRangeNotSatisfiable {
						header.Set("Content-Range", "bytes */10")
					} else if tc.status == http.StatusPartialContent {
						header.Set("Content-Range", "bytes 0-2/10")
					}
					header.Set("Set-Cookie", "secret=1")
					header.Set("Location", "https://cdn.example.test/private")
					return &http.Response{StatusCode: tc.status, Header: header, Body: body, ContentLength: 3}, nil
				},
			}}
			h.Content(c)
			require.Equal(t, tc.want, w.Code)
			require.True(t, body.closed)
			require.Empty(t, w.Header().Get("Set-Cookie"))
			require.Empty(t, w.Header().Get("Location"))
			if w.Code < 300 {
				require.Equal(t, "mp4", w.Body.String())
				require.Equal(t, "3", w.Header().Get("Content-Length"))
				require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			} else {
				require.Contains(t, w.Body.String(), `"error":`)
				require.NotContains(t, w.Body.String(), "mp4")
			}
			if w.Code == http.StatusPartialContent {
				require.Equal(t, "bytes 0-2/10", w.Header().Get("Content-Range"))
			} else if w.Code == http.StatusRequestedRangeNotSatisfiable {
				require.Equal(t, "bytes */10", w.Header().Get("Content-Range"))
			}
		})
	}
}

func TestVideoTaskContentWaitsForSettlement(t *testing.T) {
	c, w, _ := videoTaskTestContext(http.MethodGet, "/v1/videos/vidtask_123/content", nil)
	h := &VideoTaskHandler{tasks: &videoTaskTestTasks{get: func(id string, _, _, _ int64) (videotask.Task, error) {
		return videotask.Task{ID: id, State: "pending_success", ResultURL: "https://cdn.example.test/private"}, nil
	}}}
	h.Content(c)
	require.Equal(t, http.StatusConflict, w.Code)
	require.NotContains(t, w.Body.String(), "cdn.example.test")
}

func createVideoTaskForTest(t *testing.T, h *VideoTaskHandler, c *gin.Context) {
	t.Helper()
	body, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	h.Create(c, "test-provider", videoTestModel, videoTestModel, body)
}
