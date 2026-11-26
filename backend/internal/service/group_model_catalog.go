package service

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/typesafe"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

type catalogOperation uint8

const (
	catalogInference catalogOperation = 1 << iota
	catalogImage
	catalogResponsesImage
	catalogVideo
)

type groupModelCatalog struct {
	ids []string
	ops map[string]catalogOperation
}

// GroupModelIDs projects the configuration catalog for model discovery, not
// scheduling. An empty catalog is authoritative, not a request for defaults.
func (s *GatewayService) GroupModelIDs(ctx context.Context, group *Group, platform string) ([]string, error) {
	catalog, err := s.loadGroupModelCatalog(ctx, group, platform)
	if err != nil {
		return nil, err
	}
	return projectCatalogAllowlist(catalog.ids, group.ModelAllowlist), nil
}

// GroupVideoModelIDs supplies local video names to the fixed-account OpenAI
// catalog without mixing any locally inferred text names into live discovery.
func (s *GatewayService) GroupVideoModelIDs(ctx context.Context, group *Group) ([]string, error) {
	if !group.AllowVideoGeneration && !GroupAllowsImageGeneration(group) {
		return nil, nil
	}
	catalog, err := s.loadGroupModelCatalog(ctx, group, group.Platform)
	if err != nil {
		return nil, err
	}
	return catalog.videoIDs(), nil
}

func (c groupModelCatalog) videoIDs() []string {
	var ids []string
	for _, id := range c.ids {
		if c.ops[id]&catalogVideo != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *GatewayService) loadGroupModelCatalog(ctx context.Context, group *Group, platform string) (groupModelCatalog, error) {
	var routes CompositeModelRouteRepository
	if s.compositeResolver != nil {
		routes = s.compositeResolver.repo
	}
	return loadGroupModelCatalog(ctx, s.accountRepo, routes, group, platform)
}

type catalogAccountReader interface {
	ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error)
}

type catalogRouteReader interface {
	ListByGroup(context.Context, int64, bool) ([]CompositeModelRoute, error)
}

func loadGroupModelCatalog(ctx context.Context, accounts catalogAccountReader, routes catalogRouteReader, group *Group, platform string) (groupModelCatalog, error) {
	var snapshot []Account
	if accounts != nil {
		platforms := []string{platform}
		if platform == PlatformComposite {
			platforms = domain.ConcretePlatformIDs()
		} else if platform == PlatformGemini {
			platforms = append(platforms, PlatformAntigravity)
		}
		if group.AllowVideoGeneration && platform == group.Platform {
			platforms = append(platforms, slices.Sorted(maps.Keys(providerModelCapabilities["video.generate"]))...)
		}
		var err error
		// Transient limits affect execution, not what is configured for this group.
		snapshot, err = accounts.ListModelAvailabilityCandidates(ctx, &group.ID, dedupeAndSortModelIDs(platforms), false)
		if err != nil {
			return groupModelCatalog{}, fmt.Errorf("read group model accounts: %w", err)
		}
	}
	var routeSnapshot []CompositeModelRoute
	if routes != nil && platform == group.Platform && (platform == PlatformComposite || group.AllowVideoGeneration) {
		var err error
		routeSnapshot, err = routes.ListByGroup(ctx, group.ID, false)
		if err != nil {
			if platform == PlatformComposite || ctx.Err() != nil {
				return groupModelCatalog{}, fmt.Errorf("read group model routes: %w", err)
			}
			// On concrete groups these routes are only used for videos. Keep
			// the base catalog, but don't guess which video route would win.
			withoutVideo := *group
			withoutVideo.AllowVideoGeneration = false
			return buildGroupModelCatalog(&withoutVideo, platform, snapshot, nil), nil
		}
	}
	return buildGroupModelCatalog(group, platform, snapshot, routeSnapshot), nil
}

// buildGroupModelCatalog separates enumeration from validation. A route or a
// saved exact selection can supply a name, but only a matching account can
// supply capability. The group's model allowlist is applied separately.
func buildGroupModelCatalog(group *Group, platform string, accounts []Account, routes []CompositeModelRoute) groupModelCatalog {
	eligible := catalogEligibleAccounts(group, accounts)
	// Fal participates in video ownership only. It must not claim the text
	// alias of an account that shares the same public ID.
	nonVideoOwners := slices.DeleteFunc(slices.Clone(accounts), func(a Account) bool {
		return !isConcreteRequestPlatform(a.Platform)
	})
	enabledRoutes := make([]CompositeModelRoute, 0, len(routes))
	candidates := make(map[string]struct{})
	defaultIDs := make(map[string]map[string]struct{})
	for i := range eligible {
		a := &eligible[i]
		if !catalogAccountPlatform(platform, a) && !(group.AllowVideoGeneration && platform == group.Platform && supportsVideoRoutePlatform(a.Platform)) {
			continue
		}
		mapping := a.GetModelMapping()
		for _, id := range catalogObservedIDs(a) {
			// A scoped observation can enumerate a wildcard, but it can't
			// bypass the mapping or add an ID the upstream did not report.
			if target, matched := a.ResolveMappedModel(id); matched && strings.TrimSpace(target) != "" {
				addCatalogID(candidates, id)
			}
		}
		if !catalogUsesDefaults(a, mapping) {
			for id := range mapping {
				addCatalogID(candidates, id)
			}
			continue
		}
		if _, exists := defaultIDs[a.Platform]; !exists {
			ids := make(map[string]struct{})
			for _, id := range catalogDefaultIDs(a.Platform) {
				addCatalogID(candidates, id)
				addCatalogID(ids, id)
			}
			defaultIDs[a.Platform] = ids
		}
	}
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		enabledRoutes = append(enabledRoutes, route)
		if normalizeCompositeRouteMatchType(route.MatchType) == CompositeRouteMatchExact {
			addCatalogID(candidates, route.PublicModel)
		}
	}
	// Exact selections can make wildcard mappings and prefix routes enumerable,
	// even while the allowlist is disabled. They cannot publish arbitrary names.
	for _, id := range group.ModelAllowlist.Models {
		id = strings.TrimSpace(id)
		base := catalogClaudeBaseID(id)
		if _, ok := candidates[base]; ok && (platform == PlatformAnthropic || platform == PlatformComposite) {
			addCatalogID(candidates, id)
			continue
		}
		for i := range eligible {
			if a := &eligible[i]; !catalogUsesDefaults(a, a.GetModelMapping()) {
				if _, ok := a.ResolveMappedModel(id); ok {
					addCatalogID(candidates, id)
					break
				}
			}
		}
		for _, endpoint := range catalogRouteEndpoints {
			if _, ok := matchCompositeRoute(enabledRoutes, id, endpoint); ok {
				addCatalogID(candidates, id)
				break
			}
		}
	}

	out := groupModelCatalog{ids: make([]string, 0, len(candidates)), ops: make(map[string]catalogOperation, len(candidates))}
	for _, id := range slices.Sorted(maps.Keys(candidates)) {
		ops := catalogModelOperations(group, platform, nonVideoOwners, accounts, eligible, enabledRoutes, defaultIDs, id)
		if ops != 0 {
			out.ids = append(out.ids, id)
			out.ops[id] = ops
		}
	}
	if platform == PlatformAnthropic {
		expanded := expandAnthropicLongContextModels(group, out.ids, eligible)
		for _, id := range expanded[len(out.ids):] {
			out.ops[id] = catalogInference
		}
		out.ids = expanded
	}
	return out
}

func catalogObservedIDs(a *Account) []string {
	if !a.IsGrok() || (a.Type != AccountTypeAPIKey && strings.TrimSpace(a.GetCredential("base_url")) == "") {
		return nil
	}
	// Only use a fresh observation for a locally known endpoint. Capability
	// metadata can include registry-only or mapped targets and is not a list.
	snapshot := parseGrokObservedModels(a.Extra)
	scope, err := NewGrokCatalogScope(a.ID, a.Type, buildOpenAIModelsURL(a.GetGrokBaseURL()))
	if err != nil || !grokObservedSnapshotFresh(snapshot, scope, time.Now()) {
		return nil
	}
	return snapshot.Models
}

func catalogEligibleAccounts(group *Group, accounts []Account) []Account {
	now := time.Now()
	return slices.DeleteFunc(slices.Clone(accounts), func(a Account) bool {
		// The repository also ignores expiry when reading ownership candidates.
		// Expiry with auto-pause is a configuration restriction, unlike cooldowns.
		return a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) ||
			group.RequirePrivacySet && !a.IsPrivacySet() ||
			group.RequireOAuthOnly && groupSupportsOAuthOnlyFilter(group.Platform) && a.Type == AccountTypeAPIKey
	})
}

func addCatalogID(ids map[string]struct{}, id string) {
	if id = strings.TrimSpace(id); id != "" && !strings.Contains(id, "*") {
		ids[id] = struct{}{}
	}
}

func catalogClaudeBaseID(id string) string {
	return claude.NormalizeModelID(strings.TrimSuffix(id, "-thinking"))
}

// A permissive account may accept names not present in its own catalog. That
// is not evidence to advertise a name contributed only by a different source
// (for example a fal video alias on a group with an unmapped OpenAI account).
func catalogAccountDeclares(a *Account, defaults map[string]struct{}, id string) bool {
	if !catalogUsesDefaults(a, a.GetModelMapping()) {
		_, mapped := a.ResolveMappedModel(id)
		return mapped
	}
	if _, ok := defaults[id]; ok {
		return true
	}
	if a.IsAnthropic() {
		_, ok := defaults[catalogClaudeBaseID(id)]
		return ok
	}
	return false
}

func catalogUsesDefaults(a *Account, mapping map[string]string) bool {
	return len(mapping) == 0 || a.IsOpenAIPassthroughEnabled() && !a.IsOpenAIPassthroughModelFilterEnabled()
}

func catalogDefaultIDs(platform string) []string {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformOpenCodeGo, PlatformTypeSafe:
		return defaultModelsListCandidateIDs(platform)
	case PlatformDeepseek:
		return deepseekServableModels
	case PlatformMiniMax:
		return []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5"}
	default:
		return nil
	}
}

func catalogAccountPlatform(platform string, a *Account) bool {
	return a.Platform == platform || mixedListingAccountAllowed(platform, a) ||
		platform == PlatformComposite && isConcreteRequestPlatform(a.Platform)
}

var catalogRouteEndpoints = [...]string{
	CompositeRouteEndpointAny, CompositeRouteEndpointMessages, CompositeRouteEndpointCountTokens,
	CompositeRouteEndpointResponses, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointEmbeddings,
	CompositeRouteEndpointGemini, CompositeRouteEndpointImages, CompositeRouteEndpointVideos,
}

func catalogModelOperations(group *Group, platform string, textOwners, videoOwners, accounts []Account, routes []CompositeModelRoute, defaults map[string]map[string]struct{}, id string) catalogOperation {
	var ops catalogOperation
	if platform == group.Platform && group.AllowVideoGeneration && videoModelRoutableForAccounts(group, videoOwners, accounts, routes, id) {
		ops |= catalogVideo
	}
	if platform == group.Platform && GroupAllowsImageGeneration(group) && catalogSeedanceSupported(platform, accounts, id) {
		// Seedance is a separate Ark task protocol. Its ingress currently uses
		// the image-generation gate, not the native /videos feature switch.
		ops |= catalogVideo
	}
	if platform != PlatformComposite {
		for i := range accounts {
			a := &accounts[i]
			if catalogAccountPlatform(platform, a) && (a.Platform == platform || mixedListingModelAllowed(platform, id)) &&
				catalogAccountDeclares(a, defaults[a.Platform], id) {
				ops |= catalogAccountOperations(group, a, id, id)
			}
		}
		return ops
	}

	// Each endpoint has its own winning route; never validate a shadowed route.
	// Without a route, use the same exact ownership and detector as ingress.
	owner := compositeModelOwnership(textOwners, id)
	fallback := owner.TargetPlatform
	if !owner.Matched {
		guessed, _ := DetectModelPlatform(id)
		if !owner.Ambiguous || !owner.OpenAIGatewayOAuthClaimed || !domain.UsesOpenAIGateway(guessed) {
			fallback = guessed
		}
	}
	for _, endpoint := range catalogRouteEndpoints {
		if endpoint == CompositeRouteEndpointVideos {
			continue
		}
		targetPlatform, target := fallback, id
		route, routed := matchCompositeRoute(routes, id, endpoint)
		if routed {
			targetPlatform, target = route.TargetPlatform, firstNonEmpty(strings.TrimSpace(route.UpstreamModel), id)
		}
		if !IsCompositeRouteTarget(endpoint, targetPlatform) || !catalogPlatformSupportsEndpoint(targetPlatform, endpoint) {
			continue
		}
		if endpoint == CompositeRouteEndpointGemini && (targetPlatform == PlatformAntigravity && !isAntigravityGeminiModel(target) ||
			!IsSafeGeminiModelPathSegment(id)) {
			continue
		}
		for i := range accounts {
			a := &accounts[i]
			direct := a.Platform == targetPlatform
			mixed := a.IsMixedSchedulingEnabled() && (endpoint == CompositeRouteEndpointGemini && mixedListingModelAllowed(targetPlatform, target) ||
				targetPlatform == PlatformAnthropic && endpoint != CompositeRouteEndpointGemini)
			if !direct && !mixed || !routed && !catalogAccountDeclares(a, defaults[a.Platform], id) {
				continue
			}
			// An exact ownership decision fences scheduling to accounts that
			// declared that public key, not just accounts on the same platform.
			if !routed && owner.Matched && !explicitModelMappingClaims(*a, id) {
				continue
			}
			ops |= catalogEndpointOperations(endpoint, a, catalogAccountOperations(group, a, id, target))
		}
	}
	return ops
}

func catalogSeedanceSupported(platform string, accounts []Account, id string) bool {
	if platform != PlatformOpenAI && platform != PlatformComposite {
		return false
	}
	return slices.ContainsFunc(accounts, func(a Account) bool {
		// The Seedance adapter accepts opaque Ark endpoint IDs, so these are
		// enumerable through its account mappings, not by a name prefix.
		target, mapped := a.ResolveMappedModel(id)
		return mapped && strings.TrimSpace(target) != "" && strings.TrimSpace(a.GetCredential("api_key")) != "" &&
			a.IsModelSupported(id) && a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance)
	})
}

func catalogPlatformSupportsEndpoint(platform, endpoint string) bool {
	switch endpoint {
	case CompositeRouteEndpointAny:
		// Only System One uses the catch-all path as a distinct endpoint;
		// other catch-all routes are considered when matching concrete paths.
		return platform == PlatformTypeSafe
	case CompositeRouteEndpointImages:
		return platform == PlatformOpenAI || platform == PlatformGrok
	case CompositeRouteEndpointEmbeddings:
		return platform == PlatformOpenAI
	case CompositeRouteEndpointGemini:
		return platform == PlatformGemini || platform == PlatformAntigravity
	default:
		return platform != PlatformTypeSafe
	}
}

func catalogEndpointOperations(endpoint string, account *Account, ops catalogOperation) catalogOperation {
	switch endpoint {
	case CompositeRouteEndpointImages:
		return ops & catalogImage
	case CompositeRouteEndpointGemini:
		return ops
	case CompositeRouteEndpointResponses:
		if !account.IsOpenAI() || account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) {
			return ops & (catalogInference | catalogResponsesImage)
		}
	case CompositeRouteEndpointEmbeddings:
		if account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityEmbeddings) {
			return ops & catalogInference
		}
	default:
		if !account.IsOpenAI() || account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) {
			return ops & catalogInference
		}
	}
	return 0
}

func catalogAccountOperations(group *Group, a *Account, public, model string) catalogOperation {
	if a.IsOpenCodeGo() && IsOpenCodeUnsupportedModel(model) {
		return 0
	}
	target := catalogAccountTarget(a, model)
	if target == "" {
		return 0
	}
	if a.Platform == PlatformTypeSafe {
		// System One validates the original wire ID before composite rewriting.
		if public == typesafe.JevLatestModel && target == typesafe.JevLatestModel {
			return catalogInference
		}
		return 0
	}
	return catalogTargetOperations(group, a, model, target)
}

func catalogAccountTarget(a *Account, model string) string {
	switch {
	case a.Platform == PlatformAntigravity:
		return strings.TrimSpace(mapAntigravityModel(a, model))
	case a.IsBedrock():
		if target, ok := ResolveBedrockModelID(a, model); ok {
			return target
		}
		return ""
	case a.IsAnthropic() && a.Type != AccountTypeAPIKey:
		// Anthropic OAuth and Vertex check support on the normalized wire ID.
		model = claude.NormalizeModelID(model)
		if a.Type == AccountTypeServiceAccount {
			model = normalizeVertexAnthropicModelID(model)
		}
	}
	if !a.IsModelSupported(model) {
		return ""
	}
	return strings.TrimSpace(a.GetMappedModel(model))
}

func catalogTargetOperations(group *Group, a *Account, model, target string) catalogOperation {
	switch {
	case a.Platform == PlatformFal || grokNativeVideoModel(model):
		// Native video uses its own endpoint, ownership and feature gate.
		return 0
	case a.IsOpenAI():
		// Images applies the account mapping even when Responses passthrough
		// ignores it. Evaluate the two paths independently for dual-use IDs.
		var ops catalogOperation
		responsesTarget := resolveOpenAIAccountUpstreamModelForRequest(a, model, false)
		if GroupAllowsImageGeneration(group) {
			if validateCompatibleImagesModel(target) == nil && catalogOpenAIImagesSupported(a, model, target) {
				ops |= catalogImage
			}
			if IsGPTImageGenerationModel(responsesTarget) && a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) {
				ops |= catalogResponsesImage
			}
		}
		if !grokNativeVideoModel(responsesTarget) && validateCompatibleImagesModel(model) != nil && validateCompatibleImagesModel(responsesTarget) != nil &&
			(a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) ||
				a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityEmbeddings)) {
			ops |= catalogInference
		}
		return ops
	case a.IsGrok():
		if grokNativeVideoModel(target) {
			return 0
		}
		if xai.IsGrokImagineModel(model) || xai.IsGrokImagineModel(target) {
			if GroupAllowsImageGeneration(group) && isGrokImageGenerationModel(xai.StripGrokProviderPrefix(target)) &&
				a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityGrokMediaGeneration) {
				return catalogImage
			}
			return 0
		}
		return catalogInference
	case (a.Platform == PlatformGemini || a.Platform == PlatformAntigravity) && (isGeminiImageOutputModel(model) || isGeminiImageOutputModel(target)):
		if !GeminiImageGenerationDenied(group, nil, model, target) {
			return catalogImage
		}
	default:
		return catalogInference
	}
	return 0
}

func catalogOpenAIImagesSupported(a *Account, model, target string) bool {
	if validateCompatibleImagesModel(model) != nil {
		return false
	}
	req := OpenAIImagesRequest{RequiredCapability: OpenAIImagesCapabilityBasic}
	return a.SupportsOpenAIImageCapability(req.RequiredCapabilityForModel(model)) &&
		a.SupportsOpenAIImageCapability(req.RequiredCapabilityForModel(target))
}

// Keep the existing listing order and alias matching without emitting patterns
// as IDs. Admission continues to use GroupModelAllowlist.Allows independently.
func projectCatalogAllowlist(ids []string, allowlist GroupModelAllowlist) []string {
	return allowlist.FilterForListing(ids)
}
