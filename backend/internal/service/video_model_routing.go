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

// ListVideoModels lists concrete public IDs backed by the group's video routing configuration.
func (s *GatewayService) ListVideoModels(ctx context.Context, group *Group) ([]string, error) {
	if group == nil || group.ID <= 0 || !group.AllowVideoGeneration {
		return nil, nil
	}
	accounts, err := s.videoModelAccounts(ctx, group.ID)
	if err != nil {
		return nil, err
	}
	var routes []CompositeModelRoute
	if s != nil && s.compositeResolver != nil && s.compositeResolver.repo != nil {
		routes, err = s.compositeResolver.repo.ListByGroup(ctx, group.ID, false)
		if err != nil {
			return nil, err
		}
	}
	return listVideoModelIDs(group, accounts, routes), nil
}

func listVideoModelIDs(group *Group, accounts []Account, routes []CompositeModelRoute) []string {
	candidates := make(map[string]struct{})
	add := func(model string) {
		if model = strings.TrimSpace(model); model != "" && !strings.Contains(model, "*") {
			candidates[model] = struct{}{}
		}
	}
	for _, account := range accounts {
		for model := range account.GetModelMapping() {
			add(model)
		}
	}
	for _, route := range routes {
		if normalizeCompositeRouteEndpoint(route.Endpoint) == CompositeRouteEndpointVideos &&
			normalizeCompositeRouteMatchType(route.MatchType) == CompositeRouteMatchExact {
			add(route.PublicModel)
		}
	}
	if group.ModelAllowlistEnabled() {
		// A concrete allowlist entry can make a wildcard mapping or prefix route enumerable.
		for _, model := range group.ModelAllowlist.Models {
			add(model)
		}
	}

	models := make([]string, 0, len(candidates))
	for _, model := range slices.Sorted(maps.Keys(candidates)) {
		platform, target := "", model
		if route, ok := matchCompositeRoute(routes, model, CompositeRouteEndpointVideos); ok {
			platform = route.TargetPlatform
			if upstream := strings.TrimSpace(route.UpstreamModel); upstream != "" {
				target = upstream
			}
		} else if ownership := videoModelOwnership(accounts, model); ownership.Matched {
			platform = ownership.TargetPlatform
		}
		switch platform {
		case PlatformGrok:
			if group.Platform != PlatformGrok && group.Platform != PlatformComposite {
				continue
			}
		case PlatformFal:
			if group.IsSubscriptionType() {
				continue
			}
		default:
			continue
		}
		matches := providerModelCapabilities["video.generate"][platform]
		for _, account := range accounts {
			if account.Platform == platform && matches(&account, target) {
				models = append(models, model)
				break
			}
		}
	}
	return group.ModelAllowlist.FilterForListing(models)
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
