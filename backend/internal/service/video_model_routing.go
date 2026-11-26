package service

import (
	"context"
	"maps"
	"slices"
	"strings"
)

func (s *GatewayService) videoModelAccounts(ctx context.Context, groupID int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	platforms := slices.Sorted(maps.Keys(providerModelCapabilities["video.generate"]))
	return s.accountRepo.ListModelAvailabilityCandidates(ctx, &groupID, platforms, false)
}

func (s *GatewayService) resolveVideoModelOwnership(ctx context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
	accounts, err := s.videoModelAccounts(ctx, groupID)
	if err != nil {
		return CompositeModelOwnership{}, err
	}
	return videoModelOwnership(accounts, model), nil
}

// Resolve the winning video route before checking its target account. A route
// with no usable target must not fall back to an unrelated account mapping.
func videoModelRoutableForAccounts(group *Group, ownershipAccounts, eligibleAccounts []Account, routes []CompositeModelRoute, model string) bool {
	platform, target := "", model
	if route, ok := matchCompositeRoute(routes, model, CompositeRouteEndpointVideos); ok {
		platform = route.TargetPlatform
		target = firstNonEmpty(strings.TrimSpace(route.UpstreamModel), model)
	} else if ownership := videoModelOwnership(ownershipAccounts, model); ownership.Matched {
		platform = ownership.TargetPlatform
	}
	switch platform {
	case PlatformGrok:
		if group.Platform != PlatformGrok && group.Platform != PlatformComposite {
			return false
		}
	case PlatformFal:
		if group.IsSubscriptionType() {
			return false
		}
	default:
		return false
	}
	matches := providerModelCapabilities["video.generate"][platform]
	return slices.ContainsFunc(eligibleAccounts, func(account Account) bool {
		return account.Platform == platform && matches(&account, target) &&
			(platform != PlatformGrok || account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityGrokVideoGeneration))
	})
}

func videoModelOwnership(accounts []Account, model string) CompositeModelOwnership {
	var ownership CompositeModelOwnership
	for _, account := range accounts {
		// Rate limits and busy slots affect scheduling, not model ownership.
		matches := providerModelCapabilities["video.generate"][account.Platform]
		if matches == nil || !matches(&account, model) {
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
