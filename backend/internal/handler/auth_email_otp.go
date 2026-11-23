package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type emailOTPSendRequest struct {
	Email                 string `json:"email" binding:"required,max=254"`
	TurnstileToken        string `json:"turnstile_token"`
	TencentCaptchaTicket  string `json:"tencent_captcha_ticket"`
	TencentCaptchaRandstr string `json:"tencent_captcha_randstr"`
}

type emailOTPSignupFields struct {
	InvitationCode string `json:"invitation_code" binding:"max=128"`
	PromoCode      string `json:"promo_code" binding:"max=128"`
	AffCode        string `json:"aff_code" binding:"max=128"`
}

func (f emailOTPSignupFields) codes() service.EmailOTPSignupCodes {
	return service.EmailOTPSignupCodes{Invitation: f.InvitationCode, Promo: f.PromoCode, Affiliate: f.AffCode}
}

type emailOTPVerifyRequest struct {
	Email       string `json:"email"`
	ChallengeID string `json:"challenge_id"`
	Code        string `json:"code"`
	emailOTPSignupFields
}

type emailOTPCompleteRegistrationRequest struct {
	RegistrationToken string `json:"registration_token"`
	emailOTPSignupFields
}

func (h *AuthHandler) SendEmailOTP(c *gin.Context) {
	var req emailOTPSendRequest
	if !bindEmailOTPRequest(c, &req) {
		return
	}
	result, err := h.authService.SendEmailOTP(c.Request.Context(), req.Email, captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr), servermiddleware.SecurityClientIP(c), c.GetHeader("Accept-Language"))
	if err != nil {
		if result.RetryAfter > 0 {
			c.Header("Retry-After", strconv.FormatInt(int64((result.RetryAfter+time.Second-1)/time.Second), 10))
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Accepted(c, gin.H{
		"challenge_id": result.ChallengeID,
		"expires_in":   int(service.EmailOTPCodeTTL / time.Second),
		"retry_after":  int(result.RetryAfter / time.Second),
	})
}

func (h *AuthHandler) VerifyEmailOTP(c *gin.Context) {
	var req emailOTPVerifyRequest
	if !bindEmailOTPRequest(c, &req) {
		return
	}
	result, err := h.authService.VerifyEmailOTP(c.Request.Context(), req.Email, req.ChallengeID, req.Code, req.emailOTPSignupFields.codes())
	if response.ErrorFrom(c, err) {
		return
	}
	if result.RegistrationToken != "" {
		out := gin.H{
			"requires_invitation": true,
			"registration_token":  result.RegistrationToken,
			"expires_in":          int(service.EmailOTPRegistrationTTL / time.Second),
		}
		if result.InvalidInvitation {
			out["invitation_error"] = "INVITATION_CODE_INVALID"
		}
		response.Success(c, out)
		return
	}
	h.finishEmailOTPLogin(c, result)
}

func (h *AuthHandler) CompleteEmailOTPRegistration(c *gin.Context) {
	var req emailOTPCompleteRegistrationRequest
	if !bindEmailOTPRequest(c, &req) {
		return
	}
	result, err := h.authService.CompleteEmailOTPRegistration(c.Request.Context(), req.RegistrationToken, req.emailOTPSignupFields.codes())
	if response.ErrorFrom(c, err) {
		return
	}
	h.finishEmailOTPLogin(c, result)
}

func bindEmailOTPRequest(c *gin.Context, dst any) bool {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32*1024)
	if err := c.ShouldBindJSON(dst); err != nil {
		// JSON and validation errors may include raw credential input.
		response.BadRequest(c, "Invalid request")
		return false
	}
	return true
}

func (h *AuthHandler) finishEmailOTPLogin(c *gin.Context, result service.EmailOTPResult) {
	ctx := c.Request.Context()
	if response.ErrorFrom(c, h.authService.CheckEmailOTPGeneration(ctx, result.Generation)) || response.ErrorFrom(c, ensureLoginUserActive(result.User)) {
		return
	}
	user := result.User
	// Do not make email possession a fallback around an enrolled second factor,
	// even if the global setting is temporarily off or the dependency is missing.
	if user.TotpEnabled {
		if h.totpService == nil {
			response.ErrorFrom(c, service.ErrServiceUnavailable)
			return
		}
		token, err := h.totpService.CreateEmailOTPLoginSession(ctx, user, result.Generation)
		if err != nil {
			response.ErrorFrom(c, service.ErrServiceUnavailable)
			return
		}
		response.Success(c, TotpLoginResponse{Requires2FA: true, TempToken: token, UserEmailMasked: service.MaskEmail(user.Email)})
		return
	}
	if result.Created {
		servermiddleware.SetAuditAction(c, service.AuditActionRegister)
	} else {
		h.authService.RecordSuccessfulLogin(ctx, user.ID)
	}
	servermiddleware.SetAuditActor(c, user.ID, user.Email)
	c.Set("auth_method", service.AuditAuthMethodEmailOTP)
	h.respondWithTokenPair(c, user)
}

func (h *AuthHandler) finishEmailOTPTotpLogin(c *gin.Context, session *service.TotpLoginSession, user *service.User, token string) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	// Recheck policy and the original account after TOTP. Role, email and the
	// feature switch may have changed since the first factor was accepted.
	if response.ErrorFrom(c, h.authService.CheckEmailOTPTotpLogin(ctx, session, user)) ||
		response.ErrorFrom(c, h.totpService.ConsumeEmailOTPLoginSession(ctx, token)) {
		return
	}
	h.authService.RecordSuccessfulLogin(ctx, user.ID)
	servermiddleware.SetAuditActor(c, user.ID, user.Email)
	c.Set("auth_method", service.AuditAuthMethodEmailOTP)
	h.respondWithTokenPair(c, user)
}
