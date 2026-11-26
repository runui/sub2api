package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func requestModelForTest(h *GatewayHandler, group *service.Group, modelID, etag string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Request.Header.Set("If-None-Match", etag)
	if modelID != "" {
		c.Params = gin.Params{{Key: "model", Value: modelID}}
	}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
	h.Models(c)
	return rec
}

func TestRetrieveModelMatchesVisibleCatalogue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic, service.PlatformGemini, service.PlatformGrok, service.PlatformTypeSafe, service.PlatformComposite} {
		for _, mapped := range []bool{false, true} {
			name := platform + "/fallback"
			if mapped {
				name = platform + "/mapped"
			}
			t.Run(name, func(t *testing.T) {
				accountPlatform := platform
				if platform == service.PlatformComposite {
					accountPlatform = service.PlatformOpenAI
				}
				credentials := map[string]any{}
				if mapped {
					credentials["model_mapping"] = map[string]any{"custom-model": "upstream-only-model"}
				}
				group := &service.Group{ID: 71, Platform: platform}
				h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
					group.ID: {{ID: 1, Platform: accountPlatform, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: credentials}},
				}})
				list := requestModelForTest(h, group, "", "")
				require.Equal(t, http.StatusOK, list.Code, list.Body.String())
				var catalog struct {
					Data []json.RawMessage `json:"data"`
				}
				require.NoError(t, json.Unmarshal(list.Body.Bytes(), &catalog))
				if platform == service.PlatformTypeSafe && mapped {
					// System One accepts only its fixed public ID, not arbitrary aliases.
					require.Empty(t, catalog.Data)
					require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "custom-model", "").Code)
					return
				}
				require.NotEmpty(t, catalog.Data)
				var model struct {
					ID string `json:"id"`
				}
				require.NoError(t, json.Unmarshal(catalog.Data[0], &model))
				retrieved := requestModelForTest(h, group, model.ID, "")
				require.Equal(t, http.StatusOK, retrieved.Code, retrieved.Body.String())
				require.JSONEq(t, string(catalog.Data[0]), retrieved.Body.String())
				require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "unknown-model", "").Code)
				group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"absent-from-source"}}
				hidden := requestModelForTest(h, group, model.ID, "")
				require.Equal(t, http.StatusNotFound, hidden.Code, hidden.Body.String())
				require.Contains(t, hidden.Body.String(), `"code":"model_not_found"`)
			})
		}
	}
}

func TestRetrievePinnedModelPreservesMetadataFilteringAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		selected   []string
		wantStatus int
	}{
		{name: "metadata", wantStatus: http.StatusOK},
		{name: "hidden", selected: []string{"other-model"}, wantStatus: http.StatusNotFound},
		{name: "upstream failure", status: http.StatusServiceUnavailable, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{
				2: `{"data":[{"id":"special-model","ID":"not-a-model-id","owned_by":"source-owner","created":123,"extra":{"context":999}}]}`,
			}, statuses: map[int64]int{}}
			if tc.status != 0 {
				upstream.statuses[2] = tc.status
			}
			codex := newPinnedCodexTestHandler([]service.Account{newPinnedCodexAccount(2, service.StatusActive, true, false)}, upstream, 3)
			h := &GatewayHandler{openAIGatewayService: codex.gatewayService, maxAccountSwitches: 3}
			group := &service.Group{ID: 72, Platform: service.PlatformOpenAI,
				ModelAllowlist:            service.GroupModelAllowlist{Enabled: len(tc.selected) > 0, Models: tc.selected},
				CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{2}}}
			got := requestModelForTest(h, group, "special-model", "collection-etag")
			require.Equal(t, tc.wantStatus, got.Code, got.Body.String())
			if tc.wantStatus == http.StatusOK {
				list := requestModelForTest(h, group, "", "")
				var catalog struct {
					Data []json.RawMessage `json:"data"`
				}
				require.NoError(t, json.Unmarshal(list.Body.Bytes(), &catalog))
				require.JSONEq(t, string(catalog.Data[0]), got.Body.String())
				require.Contains(t, got.Body.String(), `"owned_by":"source-owner"`)
				require.Contains(t, got.Body.String(), `"created":123`)
				again := requestModelForTest(h, group, "special-model", list.Header().Get("ETag"))
				require.Equal(t, http.StatusOK, again.Code)
				require.Empty(t, again.Header().Get("ETag"))
				require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "not-a-model-id", "").Code)
			}
		})
	}
}

func TestRetrievePinnedModelMediaProjectionUpdatesCollectionETag(t *testing.T) {
	group := &service.Group{ID: 73, Platform: service.PlatformOpenAI, AllowVideoGeneration: true, AllowImageGeneration: true,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{2}}}
	upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{
		2: `{"data":[{"id":"text-model","owned_by":"upstream"},{"id":"gpt-image-1"}]}`,
	}}
	codex := newPinnedCodexTestHandler([]service.Account{newPinnedCodexAccount(2, service.StatusActive, true, false)}, upstream, 3)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		group.ID: {{Platform: service.PlatformFal, Type: service.AccountTypeAPIKey, Credentials: map[string]any{
			"api_key": "test", "model_mapping": map[string]any{"motion": "fal-ai/veo3.1"},
		}}},
	}})
	h.openAIGatewayService = codex.gatewayService
	first := requestModelForTest(h, group, "", "")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, []string{"text-model", "gpt-image-1", "motion"}, ordinaryPinnedModelIDs(t, first))
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)
	require.Equal(t, http.StatusNotModified, requestModelForTest(h, group, "", etag).Code)
	video := requestModelForTest(h, group, "motion", etag)
	require.Equal(t, http.StatusOK, video.Code, video.Body.String())
	require.Empty(t, video.Header().Get("ETag"))

	group.AllowVideoGeneration = false
	group.AllowImageGeneration = false
	changed := requestModelForTest(h, group, "", etag)
	require.Equal(t, http.StatusOK, changed.Code, changed.Body.String())
	require.NotEqual(t, etag, changed.Header().Get("ETag"))
	require.Equal(t, []string{"text-model"}, ordinaryPinnedModelIDs(t, changed))
	require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "motion", "").Code)
	require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "gpt-image-1", "").Code)
}
