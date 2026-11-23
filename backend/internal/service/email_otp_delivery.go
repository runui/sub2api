package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

func (s *EmailService) sendEmailOTP(ctx context.Context, email, code, locale string) error {
	templates := s.notificationEmailService
	if templates == nil {
		templates = &NotificationEmailService{settingRepo: s.settingRepo, emailService: s}
	}
	if strings.TrimSpace(locale) == "" {
		locale = templates.ResolveRecipientLocale(ctx, 0, email)
	}
	input := NotificationEmailSendInput{
		Event:          NotificationEmailEventAuthVerifyCode,
		Locale:         normalizeNotificationLocale(locale),
		RecipientEmail: email,
		RecipientName:  emailRecipientName(email),
		Variables: map[string]string{
			"verification_code":  code,
			"expires_in_minutes": strconv.Itoa(int(EmailOTPCodeTTL / time.Minute)),
		},
	}
	if err := templates.Send(ctx, input); !shouldFallbackNotificationEmail(err) {
		return err
	}
	slog.Warn("failed to load or render verification email template, falling back to official template", "recipient_hash", notificationEmailHash(email))
	official := notificationEmailOfficialTemplates[input.Event][input.Locale]
	rendered, err := renderNotificationEmail(input.Event, official.Subject, official.HTML, templates.runtimeVariables(ctx, input.Event, input.Locale, input), nil)
	if err != nil {
		return err
	}
	return s.SendEmail(ctx, email, rendered.Subject, rendered.HTML)
}
