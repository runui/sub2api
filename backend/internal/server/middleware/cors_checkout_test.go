package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCORSCheckoutHeadersDoNotChangeLegacyPolicy(t *testing.T) {
	app := gin.New()
	app.Use(CORS(config.CORSConfig{AllowedOrigins: []string{"https://client.example.com"}}))
	try := func(path, origin string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", origin)
		app.ServeHTTP(rec, req)
		return rec
	}
	for _, tt := range []struct{ path, header string }{
		{"/api/v1/payment/checkout/orders", "Idempotency-Key"},
		{"/api/v1/payment/public/checkout/orders/1/quote", "Content-Type"},
	} {
		rec := try(tt.path, "https://client.example.com")
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), tt.header)
		require.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After")
		require.Equal(t, http.StatusForbidden, try(tt.path, "https://unlisted.example.com").Code)
	}
	legacy := try("/api/v1/payment/orders", "https://client.example.com")
	require.NotContains(t, legacy.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
	require.NotContains(t, legacy.Header().Get("Access-Control-Allow-Headers"), "X-Checkout-Ticket")
}
