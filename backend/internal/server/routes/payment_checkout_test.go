package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCheckoutRoutesAreAdditiveAndNotCacheable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	router := newPaymentRoutesTestRouter(rdb)

	for _, route := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/api/v1/payment/checkout/orders", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/payment/checkout/orders/1/link", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/payment/public/checkout/orders/invalid", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/payment/public/checkout/orders/invalid/quote", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/payment/public/checkout/orders/invalid/pay", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/payment/public/checkout/orders/invalid/cancel", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/payment/public/checkout/orders/invalid/switch-method", http.StatusBadRequest},
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		require.Equal(t, route.status, recorder.Code, route.path)
		require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"), route.path)
	}
	legacy := postPublicOrderVerify(router, "198.51.100.10:1234")
	require.Equal(t, http.StatusBadRequest, legacy.Code)
	require.Empty(t, legacy.Header().Get("Cache-Control"))
}
