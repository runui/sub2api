package service

import (
	"context"
	"strings"
	"time"
)

const emailOTPTotpTokenPrefix = "eotp_"

func IsEmailOTPTotpToken(token string) bool {
	return strings.HasPrefix(token, emailOTPTotpTokenPrefix)
}

type EmailOTPTotpLogin struct {
	Generation string          `json:"generation"`
	Subject    EmailOTPSubject `json:"subject"`
}

func (s *TotpService) CreateEmailOTPLoginSession(ctx context.Context, user *User, generation string) (string, error) {
	return s.createLoginSession(ctx, &TotpLoginSession{
		UserID:   user.ID,
		Email:    user.Email,
		EmailOTP: &EmailOTPTotpLogin{Generation: generation, Subject: emailOTPSubject(user)},
	})
}

func (s *AuthService) CheckEmailOTPTotpLogin(ctx context.Context, session *TotpLoginSession, user *User) error {
	if session.EmailOTP == nil || !time.Now().Before(session.TokenExpiry) {
		return ErrEmailOTPInvalid
	}
	if err := s.CheckEmailOTPGeneration(ctx, session.EmailOTP.Generation); err != nil {
		return err
	}
	return checkEmailOTPLoginUser(user, session.Email, session.EmailOTP.Subject)
}

func (s *TotpService) ConsumeEmailOTPLoginSession(ctx context.Context, token string) error {
	// Keep legacy cache implementations unchanged, but never fall back to a
	// non-atomic get/delete when issuing a session from verified email proof.
	cache, ok := s.cache.(interface {
		ConsumeLoginSession(context.Context, string) (bool, error)
	})
	if !ok {
		return ErrServiceUnavailable
	}
	consumed, err := cache.ConsumeLoginSession(ctx, token)
	if err != nil {
		return ErrServiceUnavailable
	}
	if !consumed {
		return ErrEmailOTPInvalid
	}
	return nil
}
