package service

import (
	"context"
	"fmt"
	"html"
	"time"
)

func (s *EmailService) sendEmailOTP(ctx context.Context, email, code, locale string) error {
	siteName := "Sub2API"
	if name, err := s.settingRepo.GetValue(ctx, SettingKeySiteName); err == nil && name != "" {
		siteName = name
	}
	minutes := int(EmailOTPCodeTTL / time.Minute)
	title := "Email sign-in code"
	body := fmt.Sprintf("<p>Your sign-in code is:</p><p><strong>%s</strong></p><p>It expires %d minutes after you requested it. If you did not request this code, ignore this email.</p>", code, minutes)
	if normalizeNotificationLocale(locale) == notificationEmailLocaleChinese {
		title = "邮箱登录验证码"
		body = fmt.Sprintf("<p>您的登录验证码为：</p><p><strong>%s</strong></p><p>验证码在请求后 %d 分钟失效。如非本人操作，请忽略此邮件。</p>", code, minutes)
	}
	// Explicitly label the code as a sign-in credential. Do not use a template
	// whose text might instruct the recipient to use it for another operation.
	return s.SendEmail(ctx, email, fmt.Sprintf("[%s] %s", siteName, title), notificationEmailCard("#4f46e5", html.EscapeString(title), body))
}
