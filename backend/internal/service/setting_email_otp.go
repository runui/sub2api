package service

import (
	"context"
	"errors"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const SettingKeyEmailOTPGeneration = "email_otp_generation"

var ErrEmailOTPSMTPRequired = infraerrors.BadRequest("EMAIL_OTP_SMTP_REQUIRED", "configure SMTP before enabling email code login")

func (s *SettingService) EmailOTPEnabled(ctx context.Context) (bool, error) {
	generation, err := s.emailOTPGeneration(ctx)
	return generation != "", err
}

func (s *SettingService) emailOTPGeneration(ctx context.Context) (string, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyEmailOTPGeneration)
	if errors.Is(err, ErrSettingNotFound) {
		return "", nil
	}
	if err != nil {
		return "", ErrServiceUnavailable
	}
	return value, nil
}

func (s *SettingService) SetEmailOTPEnabled(ctx context.Context, enabled bool, email *EmailService) error {
	generation := ""
	if enabled {
		if err := email.validateEmailOTPSMTP(ctx); err != nil {
			return err
		}
		current, err := s.emailOTPGeneration(ctx)
		if err != nil {
			return err
		}
		if current != "" {
			return nil
		}
		// A new enable cycle must not revive old codes or pending registrations.
		generation, err = randomHexString(16)
		if err != nil {
			return ErrServiceUnavailable
		}
	}
	if err := s.settingRepo.Set(ctx, SettingKeyEmailOTPGeneration, generation); err != nil {
		return ErrServiceUnavailable
	}
	return nil
}

func (s *EmailService) validateEmailOTPSMTP(ctx context.Context) error {
	if s == nil {
		return ErrEmailOTPSMTPRequired
	}
	cfg, err := s.GetSMTPConfig(ctx)
	if errors.Is(err, ErrEmailNotConfigured) {
		return ErrEmailOTPSMTPRequired
	}
	if err != nil {
		return ErrServiceUnavailable
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return ErrEmailOTPSMTPRequired
	}
	// Use the mailer's own envelope validation without making a network call.
	if _, err := buildSMTPMessage(cfg, "test@example.com", "Email code login", ""); err != nil {
		return ErrEmailOTPSMTPRequired
	}
	return nil
}
