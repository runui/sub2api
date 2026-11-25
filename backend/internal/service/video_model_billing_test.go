package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
	"github.com/stretchr/testify/require"
)

func TestUnifiedVideoBillingFalUnitsAndExactAuto(t *testing.T) {
	id := "fal-ai/kling-video/v3/pro/text-to-video"
	p := falVideoProfiles[id]
	input := falvideo.Input{Duration: 5}
	group := &Group{VideoModelPrices: map[string]map[string]float64{id: {"auto": 9}}}
	for unit, want := range map[string]float64{"second": 2, "generation": .4} {
		group.VideoModelBillingRules = domain.VideoModelBillingRules{id: {Unit: unit, Rates: map[string]float64{"auto": .2}}}
		amount, err := quoteFalVideo(group, p, input, 2)
		require.NoError(t, err)
		require.Equal(t, want, amount)
	}
	group.VideoModelBillingRules[id] = domain.VideoModelBillingRule{Unit: "second", Rates: map[string]float64{"1080p": 9}}
	_, err := quoteFalVideo(group, p, input, 1)
	require.ErrorIs(t, err, ErrVideoModelPriceMissing)
	group.VideoModelBillingRules = domain.VideoModelBillingRules{}
	_, err = quoteFalVideo(group, p, input, 1)
	require.ErrorIs(t, err, ErrVideoModelPriceMissing)
}

func TestUnifiedVideoBillingLegacyProjectionAndSpecPrecedence(t *testing.T) {
	id := "fal-ai/pixverse/v5.5/text-to-video"
	group := &Group{
		VideoModelPrices:     map[string]map[string]float64{id: {"720p": 9}, "historical": {"720p": 2}},
		VideoModelSpecPrices: map[string]map[string]float64{id: {"720p-5s-16x9": .2}},
	}
	rules, conflicts := group.EffectiveVideoBillingRules()
	require.Equal(t, []string{id}, conflicts)
	require.Equal(t, .2, rules[id].SpecRates["720p-5s-16x9"])
	require.NoError(t, validateVideoBillingRules(rules, rules))
	require.ErrorContains(t, validateVideoBillingRules(rules, nil), "historical")
	rules[id] = domain.VideoModelBillingRule{Unit: "generation", Rates: map[string]float64{"720p": 1}, SpecRates: rules[id].SpecRates}
	require.NoError(t, validateVideoBillingRules(rules, rules))
	group.VideoModelBillingRules = rules
	for duration, want := range map[int]float64{5: .2, 8: 1} {
		amount, err := quoteFalVideo(group, falVideoProfiles[id], falvideo.Input{Duration: duration, Resolution: "720p", AspectRatio: "16:9"}, 1)
		require.NoError(t, err)
		require.Equal(t, want, amount)
	}
	group.VideoModelBillingRules = domain.VideoModelBillingRules{}
	rules, conflicts = group.EffectiveVideoBillingRules()
	require.NotNil(t, rules)
	require.Empty(t, rules)
	require.Empty(t, conflicts)
}

func TestUnifiedVideoBillingRecommendationsMatchCatalog(t *testing.T) {
	for _, model := range VideoBillingCatalog() {
		if r := model.Recommendation; r != nil {
			require.NotEmpty(t, r.Source)
			require.NoError(t, validateVideoBillingRules(domain.VideoModelBillingRules{model.ID: r.VideoModelBillingRule}, nil))
		}
	}
}

func TestNativeVideoRequiresMatchingModelRate(t *testing.T) {
	ctx := context.Background()
	s := &OpenAIGatewayService{}
	id := VideoPriceFamilyGrokImagineVideo
	key := &APIKey{Group: &Group{RateMultiplier: 2, VideoPrice720P: testVideoPrice(9),
		VideoModelPrices: map[string]map[string]float64{id: {"720p": .25}},
	}}
	req := GrokMediaRequestInfo{Resolution: "720p", DurationSeconds: 4}
	_, err := s.freezeGrokVideoRule(ctx, key, id, req)
	require.ErrorIs(t, err, ErrVideoGenerationDisabled)
	key.Group.AllowVideoGeneration = true
	quote, err := s.freezeGrokVideoRule(ctx, key, id, req)
	require.NoError(t, err)
	require.Equal(t, 2.0, quote.Cost.ActualCost)
	_, err = s.freezeGrokVideoRule(ctx, key, VideoPriceFamilyGrokImagineVideo15, req)
	require.ErrorIs(t, err, ErrVideoModelPriceMissing)
	_, err = s.freezeGrokVideoRule(ctx, key, id, GrokMediaRequestInfo{Resolution: "1080p"})
	require.ErrorIs(t, err, ErrVideoModelPriceMissing)
	key.Group.VideoModelBillingRules = domain.VideoModelBillingRules{}
	_, err = s.freezeGrokVideoRule(ctx, key, id, req)
	require.ErrorIs(t, err, ErrVideoModelPriceMissing)
}

func TestUnifiedVideoBillingGrokFrozenQuoteSurvivesPriceAndDurationChanges(t *testing.T) {
	ctx := context.Background()
	s := &OpenAIGatewayService{billingService: &BillingService{}}
	id := VideoPriceFamilyGrokImagineVideo
	key := &APIKey{Group: &Group{RateMultiplier: 2, AllowVideoGeneration: true, VideoModelBillingRules: domain.VideoModelBillingRules{
		id: {Unit: "generation", Rates: map[string]float64{"720p": .5}},
	}}}
	quote, err := s.freezeGrokVideoRule(ctx, key, id, GrokMediaRequestInfo{DurationSeconds: 5, Resolution: "720p"})
	require.NoError(t, err)
	require.Equal(t, 1.0, quote.Cost.ActualCost)
	payload, err := json.Marshal(GrokVideoPendingBilling{VideoBillingQuote: quote, UnifiedVideoBilling: true})
	require.NoError(t, err)
	var pending GrokVideoPendingBilling
	require.NoError(t, json.Unmarshal(payload, &pending))
	key.Group.VideoModelBillingRules[id] = domain.VideoModelBillingRule{Unit: "second", Rates: map[string]float64{"720p": 9}}
	result := &OpenAIForwardResult{Model: id, VideoCount: 1, VideoDurationSeconds: 15, VideoResolution: "720p", VideoBillingQuote: pending.VideoBillingQuote}
	cost, err := s.calculateOpenAIRecordUsageCost(ctx, result, key, []string{id}, 1, 1, 1, 1, UsageTokens{}, "", nil, false, openAIUsagePricingAt(nil))
	require.NoError(t, err)
	require.Equal(t, 1.0, cost.ActualCost)
	require.Equal(t, 5, pending.VideoBillingQuote.Seconds)
}
