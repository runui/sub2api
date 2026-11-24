//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFalVideoReadAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "falvid_a7306d24-992c-449e-9ec2-d29f71db8639"
	past := time.Now().Add(-time.Hour)
	for _, tt := range []struct {
		name, route, path, status string
		expires                   *time.Time
		want                      int
	}{
		{"empty balance", "/v1/videos/:request_id", "/v1/videos/" + id, service.StatusActive, nil, http.StatusNoContent},
		{"quota exhausted", "/videos/generations/:request_id/content", "/videos/generations/" + id + "/content", service.StatusAPIKeyQuotaExhausted, nil, http.StatusNoContent},
		{"expired", "/v1/videos/:request_id", "/v1/videos/" + id, service.StatusActive, &past, http.StatusForbidden},
		{"legacy task", "/v1/videos/:request_id", "/v1/videos/xai-id", service.StatusActive, nil, http.StatusForbidden},
		{"unrelated route", "/v1/other/:request_id", "/v1/other/" + id, service.StatusActive, nil, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeStandard}
			user := &service.User{ID: 1, Status: service.StatusActive}
			group := &service.Group{ID: 2, Platform: service.PlatformGrok, Status: service.StatusActive}
			key := &service.APIKey{ID: 3, UserID: user.ID, Key: "test-key", User: user, Group: group, GroupID: &group.ID, Status: tt.status, ExpiresAt: tt.expires}
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}
			keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			r := gin.New()
			r.GET(tt.route, gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)), func(c *gin.Context) { c.Status(http.StatusNoContent) })
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+key.Key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tt.want, w.Code, w.Body.String())
		})
	}
}

func TestFalVideoReplayAuthDefersOnlySpendingDenial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, status, idempotency string
		replay                    bool
		want                      int
	}{
		{"existing without YAML", service.StatusActive, "first", true, http.StatusNoContent},
		{"new or unmatched", service.StatusActive, "first", false, http.StatusForbidden},
		{"quota spent", service.StatusAPIKeyQuotaExhausted, "first", true, http.StatusNoContent},
		{"expired", service.StatusAPIKeyExpired, "first", true, http.StatusForbidden},
		{"not idempotent", service.StatusActive, "", true, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeStandard}
			user := &service.User{ID: 1, Status: service.StatusActive}
			group := &service.Group{ID: 2, Platform: service.PlatformGrok, Status: service.StatusActive}
			key := &service.APIKey{ID: 3, UserID: user.ID, Key: "test-key", User: user, Group: group, GroupID: &group.ID, Status: tt.status}
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}
			keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			r := gin.New()
			r.POST("/v1/videos/generations", gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)), func(c *gin.Context) {
				if !tt.replay && RejectFalVideoSpendingDenial(c) {
					return
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", nil)
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("Idempotency-Key", tt.idempotency)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tt.want, w.Code, w.Body.String())
		})
	}
}
