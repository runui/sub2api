package routes

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/middleware"
	"github.com/gin-gonic/gin"
)

func registerEmailOTPAuthRoutes(auth *gin.RouterGroup, h *handler.AuthHandler, limiter *middleware.RateLimiter) {
	group := auth.Group("/email-otp")
	limits := middleware.RateLimitOptions{FailureMode: middleware.RateLimitFailUnavailable, IPv6PrefixBits: 64}
	group.POST("/send", limiter.LimitWithOptions("auth-email-otp-send", 5, time.Minute, limits), h.SendEmailOTP)
	group.POST("/verify", limiter.LimitWithOptions("auth-email-otp-verify", 20, time.Minute, limits), h.VerifyEmailOTP)
	group.POST("/complete-registration", limiter.LimitWithOptions("auth-email-otp-register", 10, time.Minute, limits), h.CompleteEmailOTPRegistration)
}
