package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncFalVideoModelCatalog(t *testing.T) {
	account := &Account{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": " fal-test-key ", "base_url": "https://unused.example",
	}}
	for _, tt := range []struct {
		name       string
		statusCode int
		body       string
		wantModels []string
		wantErr    string
	}{
		{
			name: "only supported gateway model", statusCode: http.StatusOK,
			body: fmt.Sprintf(`{"models":[{"endpoint_id":%q,"metadata":{"status":"active"}},{"endpoint_id":%q,"metadata":{"status":"active"}},{"endpoint_id":"other/video","metadata":{"status":"active"}}]}`,
				falVideoTextEndpoint, falVideoImageEndpoint),
			wantModels: []string{falVideoUpstreamModel},
		},
		{
			name: "requires both active endpoints", statusCode: http.StatusOK,
			body: fmt.Sprintf(`{"models":[{"endpoint_id":%q,"metadata":{"status":"active"}},{"endpoint_id":%q,"metadata":{"status":"deprecated"}}]}`,
				falVideoTextEndpoint, falVideoImageEndpoint),
			wantModels: []string{},
		},
		{name: "empty catalog", statusCode: http.StatusOK, body: `{"models":[]}`, wantModels: []string{}},
		{name: "invalid catalog", statusCode: http.StatusOK, body: `{}`, wantErr: "Invalid fal model catalog response"},
		{name: "upstream error", statusCode: http.StatusUnauthorized, body: `{"error":"private details"}`, wantErr: "Upstream model list request failed with HTTP 401"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: tt.statusCode, Body: io.NopCloser(strings.NewReader(tt.body)),
			}}
			svc := &AccountTestService{httpUpstream: upstream}
			catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
			if tt.wantErr != "" {
				var syncErr *UpstreamModelSyncError
				require.ErrorAs(t, err, &syncErr)
				require.Equal(t, tt.wantErr, syncErr.SafeMessage())
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantModels, catalog.Models)
			require.Equal(t, "upstream", catalog.DiscoverySource)
			require.Empty(t, catalog.Warnings)
			require.Len(t, upstream.requests, 1)
			req := upstream.lastReq
			require.Equal(t, http.MethodGet, req.Method)
			require.Equal(t, "api.fal.ai", req.URL.Host)
			require.Equal(t, "/v1/models", req.URL.Path)
			require.Equal(t, "100", req.URL.Query().Get("limit"))
			require.Equal(t, "recent", req.URL.Query().Get("sort"))
			require.Equal(t, "Key fal-test-key", req.Header.Get("Authorization"))
			require.True(t, HTTPUpstreamPublicHostsOnly(req.Context()))
			require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
		})
	}
}

func TestFalCatalogPaginatesAndDoesNotReturnPartialResults(t *testing.T) {
	account := &Account{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}
	page1 := `{"models":[{"endpoint_id":"fal-ai/veo3.1","metadata":{"status":"active","category":"text-to-video"}}],"has_more":true,"next_cursor":"page 2"}`
	for _, tc := range []struct {
		name, page2 string
		fail        bool
	}{
		{"complete", `{"models":[{"endpoint_id":"other/edit","metadata":{"status":"active","category":"video-to-video"}},{"endpoint_id":"other/unknown","metadata":{"status":"active","category":"unverified-to-video"}}]}`, false},
		{"repeated cursor", page1, true},
		{"invalid second page", `{}`, true},
		{"empty second page", `{"models":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{falVideoJSON(page1), falVideoJSON(tc.page2)}}
			catalog, err := (&AccountTestService{httpUpstream: upstream}).SyncUpstreamModelCatalog(context.Background(), account)
			require.Len(t, upstream.requests, 2)
			require.Equal(t, "page 2", upstream.requests[1].URL.Query().Get("cursor"))
			if tc.fail {
				require.Error(t, err)
				require.Nil(t, catalog)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{falVideoVeo31}, catalog.Models)
			require.Equal(t, "unsupported_video_input", catalog.Capabilities[1].Reason)
			require.Equal(t, "unknown_capability", catalog.Capabilities[2].Reason)
			require.Empty(t, catalog.Capabilities[2].Operations)
		})
	}
}
