package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

func (s *GatewayService) resolveVideoModelOwnership(ctx context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
	if s.accountRepo == nil {
		return CompositeModelOwnership{}, nil
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, &groupID, []string{PlatformGrok, PlatformFal}, false)
	if err != nil {
		return CompositeModelOwnership{}, err
	}
	return videoModelOwnership(accounts, model), nil
}

func videoModelOwnership(accounts []Account, model string) CompositeModelOwnership {
	var ownership CompositeModelOwnership
	for _, account := range accounts {
		// Rate limits and busy slots affect scheduling, not model ownership.
		switch account.Platform {
		case PlatformGrok:
			if enabled, set := grokMediaEligibilityOverride(account.Extra); set && !enabled {
				continue
			}
			if !account.SupportsNativeVideoModel(model) {
				continue
			}
		case PlatformFal:
			if _, ok := falVideoAccountModel(&account, model); !ok {
				continue
			}
		default:
			continue
		}
		if ownership.Matched && ownership.TargetPlatform != account.Platform {
			return CompositeModelOwnership{Ambiguous: true}
		}
		ownership.TargetPlatform, ownership.Matched = account.Platform, true
	}
	return ownership
}

func supportsVideoRoutePlatform(platform string) bool {
	return platform == PlatformGrok || platform == PlatformFal
}

func (a *Account) SupportsNativeVideoModel(model string) bool {
	if a == nil || a.Platform != PlatformGrok || model == "" || !a.IsModelSupported(model) {
		return false
	}
	switch xai.CanonicalImagineVideoModel(a.GetMappedModel(model)) {
	case xai.DefaultImagineVideoModel, xai.DefaultImagineVideo15Model:
		return true
	}
	return false
}
