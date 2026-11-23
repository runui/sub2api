package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

func (s *AuthService) checkEmailSignupOpen(ctx context.Context, email string) error {
	if !s.IsRegistrationEnabled(ctx) {
		return ErrRegDisabled
	}
	// Synthetic OAuth addresses must never collide with local accounts.
	if isReservedEmail(email) {
		return ErrEmailReserved
	}
	return nil
}

func (s *AuthService) emailSignupInvitation(ctx context.Context, code string) (*RedeemCode, error) {
	if !s.settingService.IsInvitationCodeEnabled(ctx) {
		return nil, nil
	}
	return s.lookupEmailSignupInvitation(ctx, code)
}

func (s *AuthService) lookupEmailSignupInvitation(ctx context.Context, code string) (*RedeemCode, error) {
	if code == "" {
		return nil, ErrInvitationCodeRequired
	}
	if s.redeemRepo == nil {
		return nil, ErrServiceUnavailable
	}
	invitation, err := s.redeemRepo.GetByCode(ctx, code)
	if err != nil || invitation == nil || invitation.Type != RedeemTypeInvitation || !invitation.CanUse() {
		return nil, ErrInvitationCodeInvalid
	}
	return invitation, nil
}

func (s *AuthService) checkEmailSignupAvailability(ctx context.Context, email string) error {
	if err := s.checkEmailSignupUniqueness(ctx, email); err != nil {
		return err
	}
	return s.validateRegistrationEmailQuota(ctx, email)
}

func (s *AuthService) checkEmailSignupUniqueness(ctx context.Context, email string) error {
	// Alias folding is for deduplication only; it does not establish account ownership.
	exists, err := s.existsByEmailOrAlias(ctx, email)
	if err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Database error checking email exists: %v", err)
		return ErrServiceUnavailable
	}
	if exists {
		return ErrEmailExists
	}
	return nil
}

// createEmailSignupUser is shared only after the caller applies its own proof
// and registration checks. Availability and invitation use are enforced again
// by the write path, so neither a race nor an early policy check can grant twice.
func (s *AuthService) createEmailSignupUser(ctx context.Context, email, password string, invitation *RedeemCode, promoCode, affiliateCode string, domainPolicy *registrationEmailDomainPolicy) (*User, error) {
	hashedPassword, err := s.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	grantPlan := s.resolveSignupGrantPlan(ctx, "email")
	user := &User{
		Email:        email,
		PasswordHash: hashedPassword,
		Role:         RoleUser,
		Balance:      grantPlan.Balance,
		Concurrency:  grantPlan.Concurrency,
		RPMLimit:     s.settingService.GetDefaultUserRPMLimit(ctx),
		Status:       StatusActive,
	}
	if err := s.createUserAndClaimInvitation(ctx, user, invitation, domainPolicy); err != nil {
		switch {
		case errors.Is(err, ErrEmailExists):
			return nil, ErrEmailExists
		case errors.Is(err, ErrEmailDomainRegistrationLimit):
			return nil, ErrEmailDomainRegistrationLimit
		case errors.Is(err, ErrInvitationCodeInvalid):
			return nil, ErrInvitationCodeInvalid
		case domainPolicy != nil && errors.Is(err, ErrEmailSuffixNotAllowed):
			return nil, err
		default:
			logger.LegacyPrintf("service.auth", "[Auth] Database error creating user: %v", err)
			return nil, ErrServiceUnavailable
		}
	}

	// Provision only after the account and invitation claim are committed. These
	// existing best-effort grants must not turn a committed account into a retry.
	s.postAuthUserBootstrap(ctx, user, "email", true)
	s.assignSubscriptions(ctx, user.ID, grantPlan.Subscriptions, "auto assigned by signup defaults")
	_ = s.snapshotPlatformQuotaDefaults(ctx, user.ID, &grantPlan)
	if s.affiliateService != nil {
		if _, err := s.affiliateService.EnsureUserAffiliate(ctx, user.ID); err != nil {
			logger.LegacyPrintf("service.auth", "[Auth] Failed to initialize affiliate profile for user %d: %v", user.ID, err)
		}
		if code := strings.TrimSpace(affiliateCode); code != "" {
			if err := s.affiliateService.BindInviterByCode(ctx, user.ID, code); err != nil {
				logger.LegacyPrintf("service.auth", "[Auth] Failed to bind affiliate inviter for user %d: %v", user.ID, err)
			}
		}
	}
	if promoCode != "" && s.promoService != nil && s.settingService.IsPromoCodeEnabled(ctx) {
		if err := s.promoService.ApplyPromoCode(ctx, user.ID, promoCode); err != nil {
			logger.LegacyPrintf("service.auth", "[Auth] Failed to apply promo code for user %d: %v", user.ID, err)
		} else if updated, err := s.userRepo.GetByID(ctx, user.ID); err == nil {
			user = updated
		}
	}
	return user, nil
}
