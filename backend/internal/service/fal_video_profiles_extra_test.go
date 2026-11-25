package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestFalVideoExtraProfilesSubmitAndSettle(t *testing.T) {
	const image = "https://example.test/frame.png"
	for _, tc := range []struct {
		endpoint, fields, priceKey string
		generation                 bool
		seconds                    int
	}{
		{"wan/v2.6/text-to-video", `"duration":"5","resolution":"720p","aspect_ratio":"16:9","multi_shots":false`, "720p", false, 5},
		{"wan/v2.6/image-to-video", `"duration":"5","resolution":"720p","image_url":"` + image + `","multi_shots":false`, "720p", false, 5},
		{"wan/v2.6/image-to-video/flash", `"duration":"5","resolution":"720p","image_url":"` + image + `","multi_shots":false,"generate_audio":false`, "720p", false, 5},
		{"fal-ai/kling-video/v3/standard/text-to-video", `"duration":"5","aspect_ratio":"16:9","generate_audio":false`, "auto", false, 5},
		{"fal-ai/kling-video/v3/standard/image-to-video", `"duration":"5","start_image_url":"` + image + `","generate_audio":false`, "auto", false, 5},
		{"fal-ai/kling-video/v3/pro/text-to-video", `"duration":"5","aspect_ratio":"16:9","generate_audio":false`, "auto", false, 5},
		{"fal-ai/kling-video/v3/pro/image-to-video", `"duration":"5","start_image_url":"` + image + `","generate_audio":false`, "auto", false, 5},
		{"fal-ai/kling-video/v2.6/pro/text-to-video", `"duration":"5","aspect_ratio":"16:9","generate_audio":false`, "auto", false, 5},
		{"fal-ai/kling-video/v2.6/pro/image-to-video", `"duration":"5","start_image_url":"` + image + `","generate_audio":false`, "auto", false, 5},
		{"fal-ai/minimax/hailuo-02/pro/text-to-video", "", "1080p", false, 6},
		{"fal-ai/minimax/hailuo-02/pro/image-to-video", `"image_url":"` + image + `"`, "1080p", false, 6},
		{"bytedance/seedance-2.0/text-to-video", seedanceTestFields, "720p-5s-16x9", true, 5},
		{"bytedance/seedance-2.0/image-to-video", seedanceTestFields + `,"image_url":"` + image + `"`, "720p-5s-16x9", true, 5},
		{"bytedance/seedance-2.0/fast/text-to-video", seedanceTestFields, "720p-5s-16x9", true, 5},
		{"bytedance/seedance-2.0/fast/image-to-video", seedanceTestFields + `,"image_url":"` + image + `"`, "720p-5s-16x9", true, 5},
		{"fal-ai/pixverse/v5.5/text-to-video", pixverseTestFields + `,"aspect_ratio":"16:9"`, "720p-5s-16x9", true, 5},
		{"fal-ai/pixverse/v5.5/image-to-video", pixverseTestFields + `,"image_url":"` + image + `"`, "720p-5s", true, 5},
		{"fal-ai/ltx-2.3-quality/text-to-video", ltxTestFields, "720p-5s-16x9", true, 5},
		{"fal-ai/ltx-2.3-quality/image-to-video", ltxTestFields + `,"image_url":"` + image + `"`, "720p-5s-16x9", true, 5},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			ctx := context.Background()
			s, store, billing, usage, req := falVideoFixture()
			account := s.accounts.(falVideoTestAccounts).account
			account.Credentials["model_mapping"] = map[string]any{req.Model: tc.endpoint}
			if strings.Contains(tc.fields, image) {
				req.ImageURL = image
			}
			wrongMode := req
			wrongMode.ImageURL = image
			if req.ImageURL != "" {
				wrongMode.ImageURL = ""
			}
			_, _, body, err := prepareFalVideo(tc.endpoint, wrongMode)
			require.ErrorIs(t, err, ErrVideoTaskInvalid)
			require.Empty(t, body)
			_, err = s.Submit(ctx, req)
			require.ErrorIs(t, err, ErrVideoModelPriceMissing)
			require.Empty(t, store.task.ID)
			require.Empty(t, billing.reserved)

			group := req.APIKey.Group
			group.VideoRateIndependent, group.VideoRateMultiplier = true, 2
			amount := 0.5
			rule := domain.VideoModelBillingRule{Unit: domain.VideoBillingPerGeneration}
			if tc.generation {
				rule.SpecRates = map[string]float64{tc.priceKey: .25}
			} else {
				rule.Unit = domain.VideoBillingPerSecond
				rule.Rates = map[string]float64{tc.priceKey: .25}
				amount *= float64(tc.seconds)
			}
			group.VideoModelBillingRules = domain.VideoModelBillingRules{tc.endpoint: rule}
			require.NoError(t, validateVideoBillingRules(group.VideoModelBillingRules, nil))
			queue := "https://queue.fal.run/" + tc.endpoint
			job := "https://queue.fal.run/" + strings.Join(strings.Split(tc.endpoint, "/")[:2], "/") + "/requests/job-1"
			calls := 0
			s.transport = falVideoTestTransport{do: func(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				switch r.URL.String() {
				case queue:
					require.Equal(t, http.MethodPost, r.Method)
					var body json.RawMessage
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					fields := `"prompt":"` + req.Prompt + `"`
					if tc.fields != "" {
						fields += "," + tc.fields
					}
					require.JSONEq(t, "{"+fields+"}", string(body))
					require.Len(t, billing.reserved, 1)
					require.Equal(t, amount, billing.reserved[0].QuotedAmountUSD)
					return falVideoJSON(`{"request_id":"job-1","status_url":"` + job + `/status","response_url":"` + job + `"}`), nil
				case job + "/status":
					return falVideoJSON(`{"status":"COMPLETED","request_id":"job-1"}`), nil
				case job:
					return falVideoJSON(`{"video":{"url":"` + falVideoTestCDN + `"}}`), nil
				default:
					t.Fatalf("unexpected request: %s", r.URL)
					return nil, nil
				}
			}}
			ack, err := s.Submit(ctx, req)
			require.NoError(t, err)
			require.Equal(t, tc.endpoint, ack.Endpoint)
			require.Equal(t, tc.seconds, ack.Duration)
			require.NotEmpty(t, ack.ProfileVersion)
			require.Equal(t, "video", ack.ResultPath)
			group.VideoModelBillingRules = domain.VideoModelBillingRules{}
			group.AllowVideoGeneration = false
			replay, err := s.Submit(ctx, req)
			require.NoError(t, err)
			require.Equal(t, ack.ID, replay.ID)
			require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
			require.NoError(t, advanceFalVideoFixture(ctx, t, s, store))
			require.Equal(t, "succeeded", store.task.State)
			require.Equal(t, falVideoTestCDN, store.task.ResultURL)
			require.Equal(t, []bool{true}, billing.settled)
			require.Len(t, usage.logs, 1)
			require.Equal(t, amount, usage.logs[0].ActualCost)
			require.Equal(t, 3, calls)
		})
	}
}

const seedanceTestFields = `"duration":"5","resolution":"720p","aspect_ratio":"16:9","generate_audio":false`
const pixverseTestFields = `"duration":"5","resolution":"720p","generate_audio_switch":false,"generate_multi_clip_switch":false`
const ltxTestFields = `"num_frames":121,"frames_per_second":24,"resolution":{"width":1280,"height":720},"generate_audio":false,"sync_mode":false`

func TestFalVideoExtraProfilesRejectUnsupportedSpecs(t *testing.T) {
	const image = "https://example.test/frame.png"
	for _, tc := range []struct {
		endpoint string
		input    VideoTaskRequest
	}{
		{"wan/v2.6/image-to-video", VideoTaskRequest{ImageURL: image, AspectRatio: "16:9"}},
		{"wan/v2.6/text-to-video", VideoTaskRequest{Resolution: "480p"}},
		{"fal-ai/kling-video/v3/pro/text-to-video", VideoTaskRequest{Resolution: "1080p"}},
		{"fal-ai/kling-video/v2.6/pro/text-to-video", VideoTaskRequest{Duration: 3}},
		{"fal-ai/minimax/hailuo-02/pro/text-to-video", VideoTaskRequest{Duration: 5}},
		{"bytedance/seedance-2.0/image-to-video", VideoTaskRequest{ImageURL: image, AspectRatio: "auto"}},
		{"fal-ai/pixverse/v5.5/text-to-video", VideoTaskRequest{Duration: 10, Resolution: "1080p"}},
		{"fal-ai/ltx-2.3-quality/image-to-video", VideoTaskRequest{ImageURL: image, AspectRatio: "9:16"}},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			tc.input.Prompt = "an ocean"
			_, _, body, err := prepareFalVideo(tc.endpoint, tc.input)
			require.ErrorIs(t, err, ErrVideoTaskInvalid)
			require.Empty(t, body)
		})
	}
}

func TestFalVideoProjectionRechecksStoredDiscovery(t *testing.T) {
	const wan = "wan/v2.6/text-to-video"
	old := &UpstreamModelCatalog{Models: []string{"retired/endpoint"}, Capabilities: []UpstreamModelCapability{
		{ID: wan, Status: "active", Reason: "not_adapted"},
		{ID: "wan/v2.6/image-to-video", Status: "deprecated", Ready: true},
		{ID: "retired/endpoint", Status: "active", Ready: true},
	}}
	got := projectFalModelCatalog(old)
	require.Equal(t, []string{wan}, got.Models)
	require.True(t, got.Capabilities[0].Ready)
	require.False(t, got.Capabilities[1].Ready)
	require.False(t, got.Capabilities[2].Ready)
	require.False(t, old.Capabilities[0].Ready)
	require.Equal(t, []string{"retired/endpoint"}, old.Models)
}
