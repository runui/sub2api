package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/stretchr/testify/require"
)

const (
	falVideoTestStatusPath = "/xai/grok-imagine-video/requests/queue-job/status"
	falVideoTestResultPath = "/xai/grok-imagine-video/requests/queue-job"
	falVideoTestCDN        = "https://v3.fal.media/files/video.mp4"
	falVideoTestKey        = "test-fal-key"
)

const falVideoTestReference = `{
	"request_id":"queue-job",
	"status_url":"https://queue.fal.run` + falVideoTestStatusPath + `",
	"response_url":"https://queue.fal.run` + falVideoTestResultPath + `"
}`

// The fixture keeps one task and exposes a clock the test can move to the next poll.
type falVideoTestStore struct {
	falVideoStore
	task      videotask.Task
	now       time.Time
	acceptErr error
}

func (s *falVideoTestStore) Create(_ context.Context, task videotask.Task) (videotask.Task, bool, error) {
	if s.task.ID != "" {
		return s.task, false, nil
	}
	task.State, task.CreatedAt = "submitting", s.now
	s.task = task
	return s.task, true, nil
}

func (s *falVideoTestStore) Get(_ context.Context, id string) (videotask.Task, error) {
	if id != s.task.ID || id == "" {
		return videotask.Task{}, sql.ErrNoRows
	}
	return s.task, nil
}

func (s *falVideoTestStore) GetByIdempotencyKey(ctx context.Context, userID, keyID, groupID int64, key string) (videotask.Task, error) {
	if s.task.UserID != userID || s.task.APIKeyID != keyID || s.task.GroupID != groupID || s.task.IdempotencyKey != key {
		return videotask.Task{}, sql.ErrNoRows
	}
	return s.Get(ctx, s.task.ID)
}

func (s *falVideoTestStore) Accept(_ context.Context, id string, ref []byte) error {
	if s.acceptErr != nil {
		return s.acceptErr
	}
	if id != s.task.ID || s.task.State != "submitting" {
		return videotask.ErrInvalidTransition
	}
	s.task.Reference = append([]byte(nil), ref...)
	s.task.State, s.task.NextPollAt = "queued", s.now
	return nil
}

func (s *falVideoTestStore) Reject(_ context.Context, id, state, code string) error {
	if id != s.task.ID || s.task.State != "submitting" {
		return videotask.ErrInvalidTransition
	}
	s.task.State, s.task.ErrorCode, s.task.NextPollAt = state, code, s.now
	return nil
}

func (s *falVideoTestStore) Claim(_ context.Context, _ string, _ int, _ time.Duration) ([]videotask.Task, error) {
	if s.task.NextPollAt.IsZero() || s.task.NextPollAt.After(s.now) || s.task.LeaseToken != "" {
		return nil, nil
	}
	s.task.LeaseToken = "test-lease"
	return []videotask.Task{s.task}, nil
}

func (s *falVideoTestStore) Reschedule(_ context.Context, id, token, state string, when time.Time) error {
	if id != s.task.ID || token == "" || token != s.task.LeaseToken {
		return videotask.ErrLeaseLost
	}
	s.task.State, s.task.LeaseToken, s.task.NextPollAt = state, "", when
	return nil
}

func (s *falVideoTestStore) Finish(_ context.Context, id, token, state, url string, duration float64, width, height int, code string) error {
	if id != s.task.ID || token == "" || token != s.task.LeaseToken {
		return videotask.ErrLeaseLost
	}
	s.task.ResultURL, s.task.ActualDuration, s.task.Width, s.task.Height = url, duration, width, height
	s.task.State, s.task.ErrorCode, s.task.LeaseToken, s.task.NextPollAt = state, code, "", time.Time{}
	if state == "pending_success" || state == "pending_failure" {
		s.task.NextPollAt = s.now
	}
	return nil
}

type falVideoTestAccounts struct {
	AccountRepository
	account *Account
}

func (a falVideoTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	if id != a.account.ID {
		return nil, sql.ErrNoRows
	}
	return a.account, nil
}

type falVideoTestTransport struct {
	HTTPUpstream
	do func(*http.Request, string, int64, int) (*http.Response, error)
}

func (f falVideoTestTransport) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return f.do(req, proxy, accountID, concurrency)
}

type falVideoTestBilling struct {
	reserved  []videotask.Task
	settled   []bool
	settleErr error
}

func (b *falVideoTestBilling) Reserve(_ context.Context, task videotask.Task) error {
	b.reserved = append(b.reserved, task)
	return nil
}

func (b *falVideoTestBilling) Settle(_ context.Context, _ videotask.Task, success bool) error {
	b.settled = append(b.settled, success)
	return b.settleErr
}

type falVideoTestUsage struct {
	UsageLogRepository
	logs []UsageLog
}

func (u *falVideoTestUsage) Create(_ context.Context, log *UsageLog) (bool, error) {
	u.logs = append(u.logs, *log)
	return true, nil
}

func falVideoFixture() (*FalVideoService, *falVideoTestStore, *falVideoTestBilling, *falVideoTestUsage, VideoTaskRequest) {
	req := VideoTaskRequest{Model: "configured-video", Prompt: "a lighthouse at dusk", UserID: 10, APIKeyID: 20, GroupID: 30, IdempotencyKey: "generation-key"}
	req.APIKey = &APIKey{Group: &Group{ID: req.GroupID, RateMultiplier: 1, VideoPrice720P: testVideoPrice(0.25), VideoPrice480P: testVideoPrice(0.125)}}
	store := &falVideoTestStore{now: time.Now()}
	billing, usage := &falVideoTestBilling{}, &falVideoTestUsage{}
	s := &FalVideoService{
		store: store, billing: billing, usage: usage,
		accounts: falVideoTestAccounts{account: &Account{ID: 40, Platform: PlatformFal, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 3, Credentials: map[string]any{"api_key": falVideoTestKey, "model_mapping": map[string]any{req.Model: falVideoUpstreamModel}}}},
	}
	return s, store, billing, usage, req
}

func falVideoJSON(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestFalVideoProfileSubmissionPollReplayAndFrozenQuote(t *testing.T) {
	for _, tc := range []struct{ endpoint, image, aspect string }{
		{falVideoVeo31, "", "16:9"},
		{falVideoVeo31 + "/image-to-video", "https://example.test/frame.png", "auto"},
		{falVideoVeo31Fast, "", "16:9"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			ctx := context.Background()
			s, store, billing, usage, req := falVideoFixture()
			s.gateway = &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{FalVideoProfilesEnabled: true}}}
			req.APIKey.Group.VideoRateIndependent = true
			req.APIKey.Group.VideoRateMultiplier = 1
			account := s.accounts.(falVideoTestAccounts).account
			account.Credentials["model_mapping"] = map[string]any{req.Model: tc.endpoint}
			req.ImageURL, req.Duration, req.Resolution = tc.image, 4, "4k"
			req.APIKey.Group.VideoModelPrices = NormalizeVideoModelPrices(map[string]map[string]float64{tc.endpoint: {"4K": 0.75}})
			calls := 0
			queue := "https://queue.fal.run/" + tc.endpoint
			job := queue + "/requests/job-1"
			s.transport = falVideoTestTransport{do: func(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				switch r.URL.String() {
				case queue:
					require.Equal(t, http.MethodPost, r.Method)
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					expected := map[string]any{"prompt": req.Prompt, "duration": "4s", "resolution": "4k", "aspect_ratio": tc.aspect, "generate_audio": false}
					if tc.image != "" {
						expected["image_url"] = tc.image
					}
					require.Equal(t, expected, body)
					require.Len(t, billing.reserved, 1)
					require.Equal(t, 3.0, billing.reserved[0].QuotedAmountUSD)
					return falVideoJSON(`{"request_id":"job-1","status_url":"` + job + `/status","response_url":"` + job + `"}`), nil
				case job + "/status":
					return falVideoJSON(`{"status":"COMPLETED","request_id":"job-1"}`), nil
				case job:
					return falVideoJSON(`{"video":{"url":"https://v3b.fal.media/a.mp4"}}`), nil
				default:
					t.Fatalf("unexpected request to %s", r.URL)
					return nil, nil
				}
			}}
			ack, err := s.Submit(ctx, req)
			require.NoError(t, err)
			require.Equal(t, tc.endpoint, ack.Endpoint)
			require.Equal(t, "veo31-v1", ack.ProfileVersion)
			delete(account.Credentials, "model_mapping")
			req.APIKey.Group.VideoModelPrices = nil
			s.gateway.cfg.Gateway.FalVideoProfilesEnabled = false
			replay, err := s.Submit(ctx, req)
			require.NoError(t, err)
			require.Equal(t, ack.ID, replay.ID)
			require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
			require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
			require.Equal(t, "succeeded", store.task.State)
			require.Equal(t, "https://v3b.fal.media/a.mp4", store.task.ResultURL)
			require.Equal(t, []bool{true}, billing.settled)
			require.Len(t, usage.logs, 1)
			require.Equal(t, 3.0, usage.logs[0].ActualCost)
			require.Equal(t, 3, calls)
		})
	}
}

func TestFalVideoProfileRequiresExactGroupPrice(t *testing.T) {
	s, store, billing, _, req := falVideoFixture()
	s.accounts.(falVideoTestAccounts).account.Credentials["model_mapping"] = map[string]any{req.Model: falVideoVeo31}
	_, err := s.Submit(context.Background(), req)
	require.ErrorContains(t, err, "positive video price")
	require.Empty(t, billing.reserved)
	require.Empty(t, store.task.ID)
}

func TestFalVideoProfileGateBlocksNewSubmissionsOnly(t *testing.T) {
	s, store, billing, _, req := falVideoFixture()
	s.gateway = &GatewayService{cfg: &config.Config{}}
	s.accounts.(falVideoTestAccounts).account.Credentials["model_mapping"] = map[string]any{req.Model: falVideoVeo31}
	req.APIKey.Group.VideoModelPrices = map[string]map[string]float64{falVideoVeo31: {"720p": 1}}
	_, err := s.Submit(context.Background(), req)
	require.ErrorIs(t, err, ErrVideoTaskTemporarilyUnavailable)
	require.Empty(t, billing.reserved)
	require.Empty(t, store.task.ID)
}

func TestFalVideoSubmitMapsInputAndPersistsBeforeAck(t *testing.T) {
	for _, tt := range []struct {
		name, endpoint, payload, image, resolution string
		duration                                   int
		price                                      float64
	}{
		{"text defaults", falVideoTextEndpoint, `{"prompt":"a lighthouse at dusk","duration":6,"resolution":"720p","aspect_ratio":"16:9"}`, "", "", 0, 0.25},
		{"image overrides", falVideoImageEndpoint, `{"prompt":"a lighthouse at dusk","image_url":"https://example.test/frame.png","duration":9,"resolution":"480p"}`, "https://example.test/frame.png", "480p", 9, 0.125},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, store, billing, _, req := falVideoFixture()
			req.ImageURL, req.Duration, req.Resolution = tt.image, tt.duration, tt.resolution
			s.transport = falVideoTestTransport{do: func(r *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "https://queue.fal.run/"+tt.endpoint, r.URL.String())
				require.Equal(t, "Key "+falVideoTestKey, r.Header.Get("Authorization"))
				require.EqualValues(t, 40, accountID)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.JSONEq(t, tt.payload, string(body))
				require.True(t, videotask.IsID(store.task.ID))
				require.Equal(t, "submitting", store.task.State)
				require.Equal(t, tt.price*float64(store.task.Duration), store.task.QuotedAmountUSD)
				require.Len(t, billing.reserved, 1)
				require.Equal(t, store.task.ID, billing.reserved[0].ID)
				return falVideoJSON(falVideoTestReference), nil
			}}
			ack, err := s.Submit(context.Background(), req)
			require.NoError(t, err)
			require.Equal(t, "queued", ack.State)
			persisted, err := store.Get(context.Background(), ack.ID)
			require.NoError(t, err)
			require.Equal(t, "queued", persisted.State)
			require.Equal(t, tt.endpoint, persisted.Endpoint)
			require.JSONEq(t, falVideoTestReference, string(persisted.Reference))
			replayed, found, err := replayVideoTask(context.Background(), store, req)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, ack.ID, replayed.ID)
			req.Prompt += " changed"
			_, _, err = replayVideoTask(context.Background(), store, req)
			require.ErrorIs(t, err, ErrVideoTaskConflict)
			req.APIKeyID++
			_, found, err = replayVideoTask(context.Background(), store, req)
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}

func TestFalVideoModelIsolationAndOwnership(t *testing.T) {
	s, store, _, _, req := falVideoFixture()
	ctx := context.Background()
	wrong := req
	wrong.RoutingModel = falVideoTextEndpoint
	_, err := s.Submit(ctx, wrong)
	require.ErrorIs(t, err, ErrVideoTaskTemporarilyUnavailable)
	wrong = req
	wrong.GroupID++
	_, err = s.Submit(ctx, wrong)
	require.ErrorIs(t, err, ErrVideoTaskNotFound)
	require.Empty(t, store.task.ID)

	tasks := &VideoTaskService{store: store}
	store.task = videotask.Task{ID: "vidtask_owned", Provider: PlatformFal, UserID: req.UserID, APIKeyID: req.APIKeyID, GroupID: req.GroupID, State: "queued"}
	got, err := tasks.Get(ctx, store.task.ID, req.UserID, req.APIKeyID, req.GroupID)
	require.NoError(t, err)
	require.Equal(t, store.task.ID, got.ID)
	for _, owner := range [][3]int64{{req.UserID + 1, req.APIKeyID, req.GroupID}, {req.UserID, req.APIKeyID + 1, req.GroupID}, {req.UserID, req.APIKeyID, req.GroupID + 1}} {
		got, err := tasks.Get(ctx, store.task.ID, owner[0], owner[1], owner[2])
		require.ErrorIs(t, err, ErrVideoTaskNotFound)
		require.Empty(t, got.ID)
	}
	_, err = tasks.Get(ctx, "vidtask_missing", req.UserID, req.APIKeyID, req.GroupID)
	require.ErrorIs(t, err, ErrVideoTaskNotFound)
}

type falVideoPoolRepo struct {
	AccountRepository
	accounts []Account
}

func (r falVideoPoolRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, _ string) ([]Account, error) {
	return r.accounts, nil
}

type falVideoSlots struct {
	ConcurrencyCache
	attempted, released []int64
}

func (s *falVideoSlots) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	s.attempted = append(s.attempted, id)
	return id == 41, nil
}

func (s *falVideoSlots) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	s.released = append(s.released, id)
	return nil
}

func TestFalVideoBusyAccountFallsBackAndQuoteSurvivesReplay(t *testing.T) {
	s, _, billing, _, req := falVideoFixture()
	first := s.accounts.(falVideoTestAccounts).account
	second := *first
	second.ID = 41
	s.accounts = falVideoPoolRepo{accounts: []Account{*first, second}}
	slots := &falVideoSlots{}
	s.concurrency = NewConcurrencyService(slots)
	req.APIKey.Group.VideoModelPrices = map[string]map[string]float64{VideoPriceFamilyGrokImagineVideo15: {"720p": 0.5}}
	req.APIKey.Group.VideoRateIndependent = true
	req.APIKey.Group.VideoRateMultiplier = 0.25
	posts := 0
	s.transport = falVideoTestTransport{do: func(_ *http.Request, _ string, id int64, _ int) (*http.Response, error) {
		posts++
		require.EqualValues(t, 41, id)
		return falVideoJSON(falVideoTestReference), nil
	}}
	ctx := context.Background()
	firstTask, err := s.Submit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []int64{40, 41}, slots.attempted)
	require.Equal(t, []int64{41}, slots.released)
	require.Equal(t, 0.75, firstTask.QuotedAmountUSD)
	req.APIKey.Group.VideoRateMultiplier = 2
	req.RoutingModel = "removed-route"
	replay, err := s.Submit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, firstTask.ID, replay.ID)
	require.Equal(t, firstTask.QuotedAmountUSD, replay.QuotedAmountUSD)
	require.Equal(t, 1, posts)
	require.Len(t, billing.reserved, 1)
}

func TestFalVideoAmbiguousSubmitIsNotRetried(t *testing.T) {
	for _, acceptFails := range []bool{false, true} {
		name := "upstream connection lost"
		if acceptFails {
			name = "queue handle could not be persisted"
		}
		t.Run(name, func(t *testing.T) {
			s, store, billing, usage, req := falVideoFixture()
			if acceptFails {
				store.acceptErr = errors.New("database unavailable")
			}
			posts := 0
			s.transport = falVideoTestTransport{do: func(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				require.Equal(t, http.MethodPost, r.Method)
				posts++
				if acceptFails {
					return falVideoJSON(falVideoTestReference), nil
				}
				return nil, io.ErrUnexpectedEOF
			}}
			ctx := context.Background()
			_, err := s.Submit(ctx, req)
			require.ErrorIs(t, err, ErrVideoTaskSubmissionUnknown)
			require.Empty(t, store.task.Reference)
			if acceptFails {
				require.Equal(t, "submitting", store.task.State)
			} else {
				require.Equal(t, "pending_failure", store.task.State)
				require.Equal(t, "submission_unknown", store.task.ErrorCode)
				require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
				require.Equal(t, "unknown", store.task.State)
				require.Equal(t, []bool{false}, billing.settled)
				require.Empty(t, usage.logs)
			}
			replayed, err := s.Submit(ctx, req)
			if acceptFails {
				require.ErrorIs(t, err, ErrVideoTaskSubmitting)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, store.task.ID, replayed.ID)
			req.Prompt += " with waves"
			_, err = s.Submit(ctx, req)
			require.ErrorIs(t, err, ErrVideoTaskConflict)
			require.Equal(t, 1, posts)
			require.Len(t, billing.reserved, 1)
		})
	}
}

func advanceFalVideoFixture(ctx context.Context, t *testing.T, s *FalVideoService, store *falVideoTestStore) error {
	t.Helper()
	jobs, err := store.Claim(ctx, PlatformFal, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	return s.advance(ctx, jobs[0])
}

func TestFalVideoCompletesAndRetriesSettlementWithoutPollingAgain(t *testing.T) {
	s, store, billing, usage, req := falVideoFixture()
	var calls []string
	status := falvideo.StatusInProgress
	s.transport = falVideoTestTransport{do: func(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		require.Equal(t, "Key "+falVideoTestKey, r.Header.Get("Authorization"))
		require.Equal(t, "queue.fal.run", r.URL.Host)
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/" + falVideoTextEndpoint:
			return falVideoJSON(falVideoTestReference), nil
		case falVideoTestStatusPath:
			body, err := json.Marshal(map[string]string{"status": status, "request_id": "queue-job"})
			require.NoError(t, err)
			return falVideoJSON(string(body)), nil
		case falVideoTestResultPath:
			return falVideoJSON(`{"video":{"url":"` + falVideoTestCDN + `","duration":4.5,"width":1280,"height":720}}`), nil
		default:
			return nil, errors.New("unexpected upstream request")
		}
	}}
	ctx := context.Background()
	ack, err := s.Submit(ctx, req)
	require.NoError(t, err)
	require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
	require.Equal(t, "running", store.task.State)
	require.True(t, store.task.NextPollAt.After(store.now))

	store.now = store.task.NextPollAt
	status = falvideo.StatusCompleted
	require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
	require.Equal(t, "pending_success", store.task.State)
	require.Equal(t, falVideoTestCDN, store.task.ResultURL)
	require.Equal(t, 4.5, store.task.ActualDuration)
	require.Empty(t, billing.settled)
	require.Empty(t, usage.logs)

	billing.settleErr = errors.New("temporary billing failure")
	require.ErrorIs(t, advanceFalVideoFixture(ctx, t, s, store), billing.settleErr)
	require.Equal(t, "pending_success", store.task.State)
	require.Equal(t, falVideoTestCDN, store.task.ResultURL)
	require.True(t, store.task.NextPollAt.After(store.now))
	require.Empty(t, usage.logs)

	billing.settleErr = nil
	store.now = store.task.NextPollAt
	require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
	require.Equal(t, "succeeded", store.task.State)
	require.Equal(t, falVideoTestCDN, store.task.ResultURL)
	require.Equal(t, 4.5, store.task.ActualDuration)
	require.Equal(t, 1280, store.task.Width)
	require.Equal(t, 720, store.task.Height)
	require.Equal(t, []bool{true, true}, billing.settled)
	require.Equal(t, []string{"POST /" + falVideoTextEndpoint, "GET " + falVideoTestStatusPath, "GET " + falVideoTestStatusPath, "GET " + falVideoTestResultPath}, calls)
	require.Len(t, usage.logs, 1)
	require.Equal(t, ack.ID, usage.logs[0].RequestID)
	require.NotNil(t, usage.logs[0].UpstreamRequestID)
	require.Equal(t, "queue-job", *usage.logs[0].UpstreamRequestID)
	require.Equal(t, 1.50, usage.logs[0].ActualCost)
}

func TestFalVideoContentUsesUncredentialedRangeRequest(t *testing.T) {
	s, _, _, _, _ := falVideoFixture()
	var calls int
	s.contentDo = func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, falVideoTestCDN, r.URL.String())
		require.Equal(t, http.Header{"Range": {"bytes=10-19"}}, r.Header)
		return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Range": {"bytes 10-19/100"}}, Body: io.NopCloser(strings.NewReader("abcdefghij"))}, nil
	}
	ctx := context.Background()
	resp, err := s.OpenContent(ctx, videotask.Task{State: "succeeded", ResultURL: falVideoTestCDN}, "bytes=10-19")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "bytes 10-19/100", resp.Header.Get("Content-Range"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "abcdefghij", string(body))
	_, err = s.OpenContent(ctx, videotask.Task{State: "pending_success", ResultURL: falVideoTestCDN}, "")
	require.ErrorIs(t, err, ErrVideoTaskConflict)
	_, err = s.OpenContent(ctx, videotask.Task{State: "succeeded", ResultURL: "https://fal.media.example.test/video.mp4"}, "")
	require.ErrorIs(t, err, ErrVideoTaskUpstream)
	require.Equal(t, 1, calls)
}

type falVideoTestContentBody struct {
	io.Reader
	closed bool
}

func (b *falVideoTestContentBody) Close() error {
	b.closed = true
	return nil
}

type falVideoTestRoundTripper func(*http.Request) (*http.Response, error)

func (f falVideoTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFalVideoContentRejectsOversizedResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPartialContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &falVideoTestContentBody{Reader: strings.NewReader("video")}
			s := &FalVideoService{contentDo: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, ContentLength: falVideoMaxContentBytes + 1, Body: body}, nil
			}}
			resp, err := s.OpenContent(context.Background(), videotask.Task{State: "succeeded", ResultURL: falVideoTestCDN}, "bytes=0-")
			require.ErrorIs(t, err, ErrVideoTaskUpstream)
			require.Nil(t, resp)
			require.True(t, body.closed)
		})
	}
}

func TestFalVideoContentDoesNotFollowRedirects(t *testing.T) {
	body := &falVideoTestContentBody{Reader: strings.NewReader("redirect")}
	calls := 0
	client := *falVideoContentClient
	client.Transport = falVideoTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, falVideoTestCDN, req.URL.String())
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": {"https://v3.fal.media/files/redirected.mp4"}},
			Body:       body,
		}, nil
	})
	s := &FalVideoService{contentDo: client.Do}
	resp, err := s.OpenContent(context.Background(), videotask.Task{State: "succeeded", ResultURL: falVideoTestCDN}, "")
	require.ErrorIs(t, err, ErrVideoTaskUpstream)
	require.Nil(t, resp)
	require.Equal(t, 1, calls)
	require.True(t, body.closed)
}

func testVideoPrice(v float64) *float64 { return &v }

func (r falVideoTestAccounts) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]Account, error) {
	if r.account == nil || r.account.Platform != platform {
		return nil, nil
	}
	return []Account{*r.account}, nil
}
