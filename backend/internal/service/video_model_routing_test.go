package service

import (
	"bytes"
	"context"
	"mime/multipart"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestVideoModelRoutingUsesOnlyVideoAccounts(t *testing.T) {
	const model = "shared-label"
	fal := Account{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key", "model_mapping": map[string]any{model: falVideoUpstreamModel},
	}}
	grok := Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"model_mapping": map[string]any{model: "grok-imagine-video-1.5-preview"},
	}}
	textOnly := Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"model_mapping": map[string]any{model: "grok-4"},
	}}
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformFal, Matched: true}, videoModelOwnership([]Account{fal, textOnly, fal}, model))
	require.True(t, videoModelOwnership([]Account{fal, grok}, model).Ambiguous)
	require.False(t, videoModelOwnership([]Account{fal}, falVideoUpstreamModel).Matched)

	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{
		{GroupID: 1, PublicModel: model, TargetPlatform: PlatformGrok, Endpoint: CompositeRouteEndpointAny, MatchType: CompositeRouteMatchExact, Enabled: true},
	}})
	resolver.videoOwnershipResolver = func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return videoModelOwnership([]Account{fal}, model), nil
	}
	ctx := context.Background()
	video, err := resolver.Resolve(ctx, 1, model, CompositeRouteEndpointVideos)
	require.NoError(t, err)
	require.Equal(t, PlatformFal, video.TargetPlatform)
	text, err := resolver.Resolve(ctx, 1, model, CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.Equal(t, PlatformGrok, text.TargetPlatform)

	resolver.repo = compositeRouteRepoStub{routes: []CompositeModelRoute{
		{GroupID: 1, PublicModel: model, UpstreamModel: "target-label", TargetPlatform: PlatformFal, Endpoint: CompositeRouteEndpointVideos, MatchType: CompositeRouteMatchExact, Enabled: true},
	}}
	video, err = resolver.Resolve(ctx, 1, model, CompositeRouteEndpointVideos)
	require.NoError(t, err)
	require.Equal(t, CompositeRouteSourceExplicit, video.Source)
	require.Equal(t, model, video.PublicModel)
	require.Equal(t, "target-label", video.UpstreamModel)
}

func TestVideoModelRoutingDoesNotGuessProvider(t *testing.T) {
	decision, err := NewCompositeRouteResolver(nil).Resolve(context.Background(), 1, "grok-imagine-video", CompositeRouteEndpointVideos)
	require.NoError(t, err)
	require.False(t, decision.Matched)
}

func TestVideoModelRoutingDisabledProfileDoesNotClaimGrokAlias(t *testing.T) {
	const model = "shared-video"
	grok := Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"model_mapping": map[string]any{model: "grok-imagine-video"},
	}}
	fal := Account{Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key", "model_mapping": map[string]any{model: falVideoVeo31},
	}}
	accounts := []Account{grok, fal}
	require.True(t, videoModelOwnershipWithProfiles(accounts, model, true).Ambiguous)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformGrok, Matched: true}, videoModelOwnershipWithProfiles(accounts, model, false))
}

func TestVideoModelSchedulerRejectsOutOfGroupAndTextAccounts(t *testing.T) {
	group := &Group{ID: 12}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{
		Matched: true, Source: CompositeRouteSourceAccount, TargetPlatform: PlatformGrok,
		PublicModel: "grok-imagine-video", UpstreamModel: "grok-imagine-video",
	})
	account := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{13}}
	const capability = OpenAIEndpointCapabilityGrokVideoGeneration
	check := func() string {
		return openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, account, PlatformGrok, "grok-imagine-video", false, capability)
	}
	require.Equal(t, "group_mismatch", check())
	account.GroupIDs = []int64{group.ID}
	require.Empty(t, check(), "a native identity model does not need an explicit mapping")
	account.Credentials = map[string]any{"model_mapping": map[string]any{"grok-imagine-video": "grok-4"}}
	require.Equal(t, "model_not_supported", check())
}

func TestVideoModelRewritePreservesMultipartImage(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "public-video"))
	file, err := writer.CreateFormFile("image", "frame.png")
	require.NoError(t, err)
	_, err = file.Write([]byte("image-bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	forward, contentType, err := rewriteGrokMediaModel(body.Bytes(), writer.FormDataContentType(), "upstream-video")
	require.NoError(t, err)
	info := ParseGrokMediaRequest(contentType, forward)
	require.Equal(t, "upstream-video", info.Model)
	require.Len(t, info.Uploads, 1)
	require.Contains(t, string(forward), "image-bytes")
}
