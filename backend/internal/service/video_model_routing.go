package service

import (
	"context"
	"maps"
	"slices"
)

func (s *GatewayService) resolveVideoModelOwnership(ctx context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
	if s.accountRepo == nil {
		return CompositeModelOwnership{}, nil
	}
	platforms := slices.Sorted(maps.Keys(providerModelCapabilities["video.generate"]))
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, &groupID, platforms, false)
	if err != nil {
		return CompositeModelOwnership{}, err
	}
	return videoModelOwnershipWithProfiles(accounts, model, s.cfg == nil || s.cfg.Gateway.FalVideoProfilesEnabled), nil
}

func videoModelOwnership(accounts []Account, model string) CompositeModelOwnership {
	return videoModelOwnershipWithProfiles(accounts, model, true)
}

func videoModelOwnershipWithProfiles(accounts []Account, model string, profilesEnabled bool) CompositeModelOwnership {
	var ownership CompositeModelOwnership
	for _, account := range accounts {
		// Rate limits and busy slots affect scheduling, not model ownership.
		matches := providerModelCapabilities["video.generate"][account.Platform]
		if matches == nil || !matches(&account, model) {
			continue
		}
		// A disabled new adapter must not claim an alias from an existing Grok route.
		if !profilesEnabled && account.Platform == PlatformFal && account.GetMappedModel(model) != falVideoUpstreamModel {
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
	return providerModelCapabilities["video.generate"][platform] != nil
}

func (a *Account) SupportsNativeVideoModel(model string) bool {
	if a == nil || a.Platform != PlatformGrok || model == "" || !a.IsModelSupported(model) {
		return false
	}
	return grokNativeVideoModel(a.GetMappedModel(model))
}
