package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	EmailOTPCodeTTL          = 5 * time.Minute
	EmailOTPResendCooldown   = time.Minute
	EmailOTPRegistrationTTL  = 5 * time.Minute
	EmailOTPMaxWrongAttempts = 5
)

var (
	ErrEmailOTPDisabled            = infraerrors.Forbidden("EMAIL_OTP_DISABLED", "email code login is disabled")
	ErrEmailOTPInvalid             = infraerrors.BadRequest("EMAIL_OTP_INVALID", "invalid or expired email code")
	ErrEmailOTPRegistrationInvalid = infraerrors.BadRequest("EMAIL_OTP_REGISTRATION_INVALID", "invalid or expired email registration token")
	ErrEmailOTPNotAllowed          = infraerrors.Forbidden("EMAIL_OTP_NOT_ALLOWED", "email code login is not allowed for this account")
	ErrEmailOTPInvalidEmail        = infraerrors.BadRequest("INVALID_EMAIL", "invalid email address")
)

// EmailOTPCache is separate from EmailCache: registration, binding and reset
// credentials must never be accepted, overwritten or consumed by this flow.
type EmailOTPCache interface {
	Issue(ctx context.Context, email string, challenge EmailOTPChallenge, ttl, cooldown time.Duration) (retryAfter time.Duration, err error)
	ChallengeRemaining(ctx context.Context, email, id, generation string) (time.Duration, error)
	MarkDelivered(ctx context.Context, email, id, generation string) error
	Discard(ctx context.Context, email, id, generation string) error
	Consume(ctx context.Context, email, id, generation, codeMAC string, maxWrong int) (*EmailOTPSubject, error)
	StoreRegistration(ctx context.Context, tokenHash string, grant EmailOTPRegistration, ttl time.Duration) error
	GetRegistration(ctx context.Context, tokenHash string) (*EmailOTPRegistration, error)
	ConsumeRegistration(ctx context.Context, tokenHash string) (bool, error)
}

type EmailOTPChallenge struct {
	ID         string          `json:"id"`
	Generation string          `json:"generation"`
	CodeMAC    string          `json:"code_mac"`
	Subject    EmailOTPSubject `json:"subject"`
}

type EmailOTPSubject struct {
	// Zero means the address was unregistered at issue time, not any account
	// that might later appear under this address.
	UserID         int64  `json:"user_id"`
	AccountVersion string `json:"account_version"`
	SignupAddress  string `json:"signup_address,omitempty"`
}

type EmailOTPRegistration struct {
	Email      string `json:"email"`
	Generation string `json:"generation"`
}

type EmailOTPSendResult struct {
	ChallengeID string
	RetryAfter  time.Duration
}

type EmailOTPSignupCodes struct {
	Invitation string
	Promo      string
	Affiliate  string
}

type EmailOTPResult struct {
	User              *User
	Generation        string
	Created           bool
	RegistrationToken string
	InvalidInvitation bool
}

func normalizeEmailOTPAddress(raw string) (string, error) {
	email := strings.TrimSpace(raw)
	if len(email) > 254 {
		return "", ErrEmailOTPInvalidEmail
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", ErrEmailOTPInvalidEmail
	}
	return email, nil
}

func validEmailOTPHex(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validEmailOTPCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

func (s *AuthService) activeEmailOTPGeneration(ctx context.Context) (string, error) {
	if s == nil || s.settingService == nil || s.emailOTPCache == nil || s.cfg == nil || s.cfg.JWT.Secret == "" {
		return "", ErrServiceUnavailable
	}
	// Read both flags together, without the backend-mode cache: an outage or a
	// stale cached value must not leave this passwordless path open.
	settings, err := s.settingService.settingRepo.GetMultiple(ctx, []string{SettingKeyEmailOTPGeneration, SettingKeyBackendModeEnabled})
	if err != nil {
		return "", ErrServiceUnavailable
	}
	generation := settings[SettingKeyEmailOTPGeneration]
	if generation == "" || settings[SettingKeyBackendModeEnabled] == "true" {
		return "", ErrEmailOTPDisabled
	}
	return generation, nil
}

func (s *AuthService) emailOTPCodeMAC(email, id, generation, code string) string {
	// The six-digit space is too small for a plain hash to protect a Redis dump.
	mac := hmac.New(sha256.New, []byte(s.cfg.JWT.Secret))
	_, _ = mac.Write([]byte("email-otp:code\x00" + generation + "\x00" + email + "\x00" + id + "\x00" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func emailOTPTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *AuthService) SendEmailOTP(ctx context.Context, rawEmail string, proof CaptchaProof, clientIP, locale string) (EmailOTPSendResult, error) {
	var result EmailOTPSendResult
	recipient, err := normalizeEmailOTPAddress(rawEmail)
	if err != nil {
		return result, err
	}
	email := strings.ToLower(recipient)
	generation, err := s.activeEmailOTPGeneration(ctx)
	if err != nil {
		return result, err
	}
	if err := s.VerifyCaptcha(ctx, proof, clientIP); err != nil {
		return result, err
	}
	if s.emailQueueService == nil || s.emailService == nil {
		return result, ErrServiceUnavailable
	}
	if err := s.emailService.validateEmailOTPSMTP(ctx); err != nil {
		return result, ErrServiceUnavailable
	}
	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return result, ErrServiceUnavailable
	}
	var subject EmailOTPSubject
	if user != nil {
		subject = emailOTPSubject(user)
		// Login lookup is case-insensitive, but SMTP must use the stored address:
		// a mail provider can distinguish local parts that differ only in case.
		recipient = strings.TrimSpace(user.Email)
	} else {
		// Preserve the verified delivery address when creating the account, so
		// future sign-ins keep reaching this mailbox on case-sensitive providers.
		subject.SignupAddress = recipient
	}
	id, err := randomHexString(16)
	if err != nil {
		return result, ErrServiceUnavailable
	}
	// A challenge without a delivered code remains unredeemable. Generate
	// the secret for every request so send does not branch on account eligibility.
	code, err := s.emailService.GenerateVerifyCode()
	if err != nil {
		return result, ErrServiceUnavailable
	}
	challenge := EmailOTPChallenge{
		ID: id, Generation: generation, Subject: subject,
		CodeMAC: s.emailOTPCodeMAC(email, id, generation, code),
	}
	retry, err := s.emailOTPCache.Issue(ctx, email, challenge, EmailOTPCodeTTL, EmailOTPResendCooldown)
	if err != nil {
		return result, ErrServiceUnavailable
	}
	if retry > 0 {
		return EmailOTPSendResult{RetryAfter: retry}, ErrVerifyCodeTooFrequent
	}

	// Enqueue every valid request the same way, including ineligible addresses.
	// Account-specific decisions must not change the public send response.
	job := &emailOTPDelivery{auth: s, email: email, recipient: recipient, id: id, generation: generation, subject: subject, code: code, locale: locale}
	if !s.emailQueueService.enqueueEmailOTP(job) {
		job.discard(ctx)
		return result, ErrServiceUnavailable
	}
	return EmailOTPSendResult{ChallengeID: id, RetryAfter: EmailOTPResendCooldown}, nil
}

func (s *AuthService) VerifyEmailOTP(ctx context.Context, rawEmail, id, code string, codes EmailOTPSignupCodes) (EmailOTPResult, error) {
	var result EmailOTPResult
	generation, err := s.activeEmailOTPGeneration(ctx)
	if err != nil {
		return result, err
	}
	email, err := normalizeEmailOTPAddress(rawEmail)
	if err != nil || !validEmailOTPHex(id, 16) || !validEmailOTPCode(code) {
		return result, ErrEmailOTPInvalid
	}
	email = strings.ToLower(email)
	subject, err := s.emailOTPCache.Consume(ctx, email, id, generation, s.emailOTPCodeMAC(email, id, generation, code), EmailOTPMaxWrongAttempts)
	if err != nil {
		return result, ErrServiceUnavailable
	}
	if subject == nil {
		return result, ErrEmailOTPInvalid
	}
	if subject.UserID != 0 {
		user, err := s.emailOTPLoginUser(ctx, email, *subject)
		return EmailOTPResult{User: user, Generation: generation}, err
	}
	if !strings.EqualFold(subject.SignupAddress, email) {
		return result, ErrEmailOTPInvalid
	}
	email = subject.SignupAddress
	if err := s.checkEmailOTPRegistration(ctx, email); err != nil {
		return result, err
	}
	invitation, err := s.emailOTPInvitation(ctx, strings.TrimSpace(codes.Invitation))
	if errors.Is(err, ErrInvitationCodeRequired) || errors.Is(err, ErrInvitationCodeInvalid) {
		token, tokenErr := s.issueEmailOTPRegistration(ctx, email, generation)
		return EmailOTPResult{RegistrationToken: token, InvalidInvitation: errors.Is(err, ErrInvitationCodeInvalid)}, tokenErr
	}
	if err != nil {
		return result, err
	}
	return s.registerEmailOTPUser(ctx, email, generation, invitation, codes)
}

func (s *AuthService) checkEmailOTPRegistration(ctx context.Context, email string) error {
	policy, err := s.emailOTPRegistrationPolicy(ctx, email)
	if err != nil {
		return err
	}
	if err := s.checkEmailSignupUniqueness(ctx, email); err != nil {
		return err
	}
	return s.validateRegistrationEmailDomainQuota(ctx, email, policy)
}

func (s *AuthService) emailOTPRegistrationPolicy(ctx context.Context, email string) (registrationEmailDomainPolicy, error) {
	var policy registrationEmailDomainPolicy
	// Read a fresh policy snapshot and fail closed on read errors. In the
	// shared SQL write path this snapshot selects the correct atomic guard;
	// a second, best-effort settings read must not silently drop the quota.
	settings, err := s.settingService.settingRepo.GetMultiple(ctx, []string{
		SettingKeyRegistrationEnabled, SettingKeyRegistrationEmailBlacklist,
		SettingKeyRegistrationEmailSuffixWhitelist, SettingKeyRegistrationEmailDomainQuotaEnabled,
	})
	if err != nil {
		return policy, ErrServiceUnavailable
	}
	if settings[SettingKeyRegistrationEnabled] != "true" {
		return policy, ErrRegDisabled
	}
	if isReservedEmail(email) {
		return policy, ErrEmailReserved
	}
	blacklist := RegistrationEmailBlacklistIdentities(ParseRegistrationEmailBlacklist(settings[SettingKeyRegistrationEmailBlacklist]))
	if _, blocked := blacklist[NormalizeEmailForAliasDedup(email)]; blocked {
		return policy, ErrEmailBlocked
	}
	return registrationEmailDomainPolicy{
		whitelist:    ParseRegistrationEmailSuffixWhitelist(settings[SettingKeyRegistrationEmailSuffixWhitelist]),
		quotaEnabled: settings[SettingKeyRegistrationEmailDomainQuotaEnabled] == "true",
	}, nil
}

func (s *AuthService) emailOTPInvitation(ctx context.Context, code string) (*RedeemCode, error) {
	value, err := s.settingService.settingRepo.GetValue(ctx, SettingKeyInvitationCodeEnabled)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		// Unlike ordinary registration's legacy bool getter, our new path
		// must not read a transient DB error as "invitation not required".
		return nil, ErrServiceUnavailable
	}
	if value != "true" {
		return nil, nil
	}
	return s.lookupEmailSignupInvitation(ctx, code)
}

func (s *AuthService) issueEmailOTPRegistration(ctx context.Context, email, generation string) (string, error) {
	token, err := randomHexString(32)
	if err != nil {
		return "", ErrServiceUnavailable
	}
	if err := s.emailOTPCache.StoreRegistration(ctx, emailOTPTokenHash(token), EmailOTPRegistration{Email: email, Generation: generation}, EmailOTPRegistrationTTL); err != nil {
		return "", ErrServiceUnavailable
	}
	return token, nil
}

func (s *AuthService) CompleteEmailOTPRegistration(ctx context.Context, token string, codes EmailOTPSignupCodes) (EmailOTPResult, error) {
	var result EmailOTPResult
	generation, err := s.activeEmailOTPGeneration(ctx)
	if err != nil {
		return result, err
	}
	if !validEmailOTPHex(token, 32) {
		return result, ErrEmailOTPRegistrationInvalid
	}
	hash := emailOTPTokenHash(token)
	grant, err := s.emailOTPCache.GetRegistration(ctx, hash)
	if err != nil {
		return result, ErrServiceUnavailable
	}
	if grant == nil || grant.Generation != generation {
		return result, ErrEmailOTPRegistrationInvalid
	}
	if err := s.checkEmailOTPRegistration(ctx, grant.Email); err != nil {
		return result, err
	}
	// Check correctable input before consuming the grant. The SQL transaction
	// still claims the invitation conditionally, against concurrent signups.
	invitation, err := s.lookupEmailSignupInvitation(ctx, strings.TrimSpace(codes.Invitation))
	if err != nil {
		return result, err
	}
	ok, err := s.emailOTPCache.ConsumeRegistration(ctx, hash)
	if err != nil {
		return result, ErrServiceUnavailable
	}
	if !ok {
		return result, ErrEmailOTPRegistrationInvalid
	}
	return s.registerEmailOTPUser(ctx, grant.Email, generation, invitation, codes)
}

func (s *AuthService) registerEmailOTPUser(ctx context.Context, email, generation string, invitation *RedeemCode, codes EmailOTPSignupCodes) (EmailOTPResult, error) {
	if err := s.CheckEmailOTPGeneration(ctx, generation); err != nil {
		return EmailOTPResult{}, err
	}
	policy, err := s.emailOTPRegistrationPolicy(ctx, email)
	if err != nil {
		return EmailOTPResult{}, err
	}
	if invitation == nil {
		// Recheck in case invitations became required after OTP verification.
		var err error
		invitation, err = s.emailOTPInvitation(ctx, strings.TrimSpace(codes.Invitation))
		if err != nil {
			return EmailOTPResult{}, err
		}
	}
	// The legacy model requires a password hash; this random password is never
	// disclosed. Never turn a create conflict into an existing-account login.
	if invitation != nil && s.entClient == nil {
		return EmailOTPResult{}, ErrServiceUnavailable
	}
	password, err := randomHexString(32)
	if err != nil {
		return EmailOTPResult{}, ErrServiceUnavailable
	}
	user, err := s.createEmailSignupUser(ctx, email, password, invitation, codes.Promo, codes.Affiliate, &policy)
	return EmailOTPResult{User: user, Created: err == nil, Generation: generation}, err
}

func (s *AuthService) CheckEmailOTPGeneration(ctx context.Context, expected string) error {
	current, err := s.activeEmailOTPGeneration(ctx)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrEmailOTPInvalid
	}
	return nil
}

func (s *AuthService) emailOTPLoginUser(ctx context.Context, email string, subject EmailOTPSubject) (*User, error) {
	user, err := s.userRepo.GetByID(ctx, subject.UserID)
	if errors.Is(err, ErrUserNotFound) {
		return nil, ErrEmailOTPInvalid
	}
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	if err := checkEmailOTPLoginUser(user, email, subject); err != nil {
		return nil, err
	}
	return user, nil
}

func checkEmailOTPLoginUser(user *User, email string, subject EmailOTPSubject) error {
	if user == nil || user.ID != subject.UserID || !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(email)) || emailOTPSubject(user) != subject {
		return ErrEmailOTPInvalid
	}
	if user.IsAdmin() || isReservedEmail(user.Email) {
		return ErrEmailOTPNotAllowed
	}
	if !user.IsActive() {
		return ErrUserNotActive
	}
	return nil
}

func emailOTPSubject(user *User) EmailOTPSubject {
	// Bind to the account's current credential state as well as the address.
	// A changed password or bound email invalidates previously issued proof.
	return EmailOTPSubject{
		UserID:         user.ID,
		AccountVersion: emailOTPTokenHash(strings.TrimSpace(user.Email) + "\x00" + user.PasswordHash),
	}
}

type emailOTPDelivery struct {
	auth       *AuthService
	email      string
	recipient  string
	id         string
	generation string
	subject    EmailOTPSubject
	code       string
	locale     string
}

func (job *emailOTPDelivery) discard(ctx context.Context) {
	// Queue/SMTP failures cannot remove a replacement issued by another node.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = job.auth.emailOTPCache.Discard(cleanup, job.email, job.id, job.generation)
}

func (q *EmailQueueService) enqueueEmailOTP(job *emailOTPDelivery) bool {
	select {
	case <-q.stopChan:
		return false
	default:
	}
	select {
	case q.taskChan <- EmailTask{emailOTP: job}:
		return true
	case <-q.stopChan:
		return false
	default:
		return false
	}
}

func (job *emailOTPDelivery) deliver(ctx context.Context) {
	s := job.auth
	if err := s.CheckEmailOTPGeneration(ctx, job.generation); err != nil {
		job.discard(ctx)
		return
	}
	if job.subject.UserID != 0 {
		if _, err := s.emailOTPLoginUser(ctx, job.email, job.subject); err != nil {
			job.discard(ctx)
			return
		}
	} else if err := s.checkEmailOTPRegistration(ctx, job.email); err != nil {
		job.discard(ctx)
		return
	}
	remaining, err := s.emailOTPCache.ChallengeRemaining(ctx, job.email, job.id, job.generation)
	if err != nil || remaining <= 0 {
		return
	}
	// Only delivered codes become usable. In-flight mail can arrive late, but
	// activation never extends the TTL or revives a replaced challenge.
	if err := s.emailService.sendEmailOTP(ctx, job.recipient, job.code, job.locale); err != nil {
		job.discard(ctx)
		// SMTP replies may contain message content. Never log the raw reply here.
		slog.Warn("email OTP delivery failed")
		return
	}
	if err := s.emailOTPCache.MarkDelivered(ctx, job.email, job.id, job.generation); err != nil {
		job.discard(ctx)
		slog.Warn("email OTP activation failed")
	}
}
