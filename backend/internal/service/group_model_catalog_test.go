package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type catalogAccountReadFunc func(context.Context, *int64, []string, bool) ([]Account, error)

func (f catalogAccountReadFunc) ListModelAvailabilityCandidates(ctx context.Context, id *int64, platforms []string, includeGrouped bool) ([]Account, error) {
	return f(ctx, id, platforms, includeGrouped)
}

type catalogRouteReadFunc func(context.Context, int64, bool) ([]CompositeModelRoute, error)

func (f catalogRouteReadFunc) ListByGroup(ctx context.Context, id int64, disabled bool) ([]CompositeModelRoute, error) {
	return f(ctx, id, disabled)
}

func TestGroupModelCatalogEmptyAndReadFailures(t *testing.T) {
	ctx := context.Background()
	group := &Group{ID: 42, Platform: PlatformOpenAI, AllowVideoGeneration: true}
	readErr := errors.New("lookup failed")
	accounts := catalogAccountReadFunc(func(_ context.Context, id *int64, platforms []string, includeGrouped bool) ([]Account, error) {
		require.Equal(t, group.ID, *id)
		require.ElementsMatch(t, []string{PlatformOpenAI, PlatformFal, PlatformGrok}, platforms)
		require.False(t, includeGrouped)
		return nil, nil
	})
	catalog, err := loadGroupModelCatalog(ctx, accounts, nil, group, group.Platform)
	require.NoError(t, err)
	require.Equal(t, []string{}, catalog.ids)
	_, err = loadGroupModelCatalog(ctx, catalogAccountReadFunc(func(context.Context, *int64, []string, bool) ([]Account, error) {
		return nil, readErr
	}), nil, group, group.Platform)
	require.ErrorIs(t, err, readErr)

	accounts = func(context.Context, *int64, []string, bool) ([]Account, error) {
		return []Account{
			{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"text": "gpt-5"}}},
			{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "model_mapping": map[string]any{"video": falVideoUpstreamModel}}},
		}, nil
	}
	routes := catalogRouteReadFunc(func(_ context.Context, id int64, disabled bool) ([]CompositeModelRoute, error) {
		require.Equal(t, group.ID, id)
		require.False(t, disabled)
		return nil, readErr
	})
	catalog, err = loadGroupModelCatalog(ctx, accounts, routes, group, group.Platform)
	require.NoError(t, err)
	require.Equal(t, []string{"text"}, catalog.ids)
	group.Platform = PlatformComposite
	_, err = loadGroupModelCatalog(ctx, accounts, routes, group, group.Platform)
	require.ErrorIs(t, err, readErr)
}

func TestGroupModelCatalogConcreteSelectionsAndTransientLimits(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	group := &Group{Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"team-z", "invented", "team-*"}}}
	accounts := []Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, RateLimitResetAt: &future, TempUnschedulableUntil: &future,
			Credentials: map[string]any{"model_mapping": map[string]any{"team-*": "gpt-5", "team-a": "gpt-5", "not-selected": "gpt-5"}}},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, ExpiresAt: &past, AutoPauseOnExpired: true},
	}
	catalog := buildGroupModelCatalog(group, group.Platform, accounts, nil)
	require.Equal(t, []string{"not-selected", "team-a", "team-z"}, catalog.ids)
	require.Equal(t, []string{"team-z", "team-a"}, projectCatalogAllowlist(catalog.ids, group.ModelAllowlist))
}

func TestGroupModelCatalogOperationsAreIndependent(t *testing.T) {
	group := &Group{Platform: PlatformComposite, AllowVideoGeneration: true, AllowImageGeneration: true}
	accounts := []Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"dual": "gpt-5", "gpt-image-1": "gpt-image-1"}}},
		{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "model_mapping": map[string]any{"video-target": falVideoUpstreamModel}}},
	}
	routes := []CompositeModelRoute{
		{PublicModel: "dual", UpstreamModel: "video-target", TargetPlatform: PlatformFal, Endpoint: CompositeRouteEndpointVideos, Enabled: true},
		{PublicModel: "shadowed", UpstreamModel: "video-target", TargetPlatform: PlatformFal, Endpoint: CompositeRouteEndpointVideos, Enabled: true, Priority: 100},
		{PublicModel: "shadowed", UpstreamModel: "missing-target", TargetPlatform: PlatformFal, Endpoint: CompositeRouteEndpointVideos, Enabled: true, Priority: 1},
		{PublicModel: "disabled", UpstreamModel: "dual", TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses},
		{PublicModel: "image-response", UpstreamModel: "gpt-image-1", TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Enabled: true},
		{PublicModel: "prefix-", MatchType: CompositeRouteMatchPrefix, UpstreamModel: "dual", TargetPlatform: PlatformOpenAI, Endpoint: CompositeRouteEndpointResponses, Enabled: true},
	}
	group.ModelAllowlist.Models = []string{"prefix-example", "nonexistent"}
	catalog := buildGroupModelCatalog(group, group.Platform, accounts, routes)
	require.Equal(t, catalogInference|catalogVideo, catalog.ops["dual"])
	require.Equal(t, catalogResponsesImage, catalog.ops["image-response"])
	require.Equal(t, []string{"dual", "gpt-image-1", "image-response", "prefix-example", "video-target"}, catalog.ids)
	group.AllowVideoGeneration = false
	group.AllowImageGeneration = false
	catalog = buildGroupModelCatalog(group, group.Platform, accounts, routes)
	require.Equal(t, []string{"dual", "prefix-example"}, catalog.ids)
	require.Equal(t, catalogInference, catalog.ops["dual"])
}

func TestGroupModelCatalogUsesOnlyScopedDiscoveryEvidence(t *testing.T) {
	group := &Group{Platform: PlatformGrok}
	account := Account{ID: 7, Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"grok-*": "grok-4"}}}
	scope, err := NewGrokCatalogScope(account.ID, account.Type, buildOpenAIModelsURL(account.GetGrokBaseURL()))
	require.NoError(t, err)
	snapshot := GrokObservedModelsSnapshot{Scope: scope, FetchedAt: time.Now().UTC().Format(time.RFC3339Nano), Models: []string{"grok-observed", "not-mapped"}}
	account.Extra = map[string]any{grokObservedModelsExtraKey: snapshot}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
		"grok-metadata-only": {ContextWindow: 100_000},
	}})
	require.Equal(t, []string{"grok-observed"}, buildGroupModelCatalog(group, group.Platform, []Account{account}, nil).ids)
	account.Credentials["base_url"] = "https://other.example"
	require.Empty(t, buildGroupModelCatalog(group, group.Platform, []Account{account}, nil).ids)
}

func TestGroupModelCatalogGrokMediaGatesCoverAliases(t *testing.T) {
	group := &Group{Platform: PlatformGrok, AllowImageGeneration: true, AllowVideoGeneration: true}
	accounts := []Account{{Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"grok-4": "grok-4", "grok-imagine-1": "grok-imagine-image-quality", "motion": "grok-imagine-video",
		}}}}
	require.Equal(t, []string{"grok-4", "grok-imagine-1", "motion"}, buildGroupModelCatalog(group, group.Platform, accounts, nil).ids)
	group.AllowImageGeneration = false
	group.AllowVideoGeneration = false
	require.Equal(t, []string{"grok-4"}, buildGroupModelCatalog(group, group.Platform, accounts, nil).ids)
	group.AllowImageGeneration = true
	group.AllowVideoGeneration = true
	accounts[0].Extra = map[string]any{GrokMediaEligibleExtraKey: false}
	require.Equal(t, []string{"grok-4"}, buildGroupModelCatalog(group, group.Platform, accounts, nil).ids)
}
