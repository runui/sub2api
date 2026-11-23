package routes

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	ratelimit "github.com/Wei-Shaw/sub2api/internal/middleware"
	"github.com/gin-gonic/gin"
)

func registerCheckoutRoutes(authenticated, public *gin.RouterGroup, h *handler.PaymentHandler, limiter *ratelimit.RateLimiter) {
	private := authenticated.Group("/checkout", checkoutResponseHeaders)
	browser := public.Group("/checkout", checkoutResponseHeaders)
	limit := func(operation string, count int) gin.HandlerFunc {
		return limiter.LimitWithOptions("checkout-"+operation, count, time.Minute, ratelimit.RateLimitOptions{
			FailureMode: ratelimit.RateLimitFailUnavailable, IPv6PrefixBits: 64,
		})
	}
	private.POST("/orders", limit("create", 20), h.CreateCheckoutOrder)
	private.GET("/orders/:id", limit("owner-read", 60), h.GetOwnedCheckoutOrder)
	private.POST("/orders/:id/link", limit("link", 30), h.ReissueCheckoutLink)
	browser.GET("/orders/:id", limit("read", 60), h.GetCheckoutOrder)
	browser.POST("/orders/:id/quote", limit("quote", 60), h.QuoteCheckoutOrder)
	browser.POST("/orders/:id/pay", limit("pay", 30), h.PayCheckoutOrder)
	browser.POST("/orders/:id/cancel", limit("cancel", 10), h.CancelUnstartedCheckout)
	browser.POST("/orders/:id/switch-method", limit("switch-method", 10), h.SwitchCheckoutMethod)
}

func checkoutResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
}
