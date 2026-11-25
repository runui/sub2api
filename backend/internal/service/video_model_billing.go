package service

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type FrozenVideoBillingQuote struct {
	Cost       CostBreakdown `json:"cost"`
	Multiplier float64       `json:"multiplier"`
	Seconds    int           `json:"seconds"`
}

func (s *OpenAIGatewayService) freezeGrokVideoRule(ctx context.Context, key *APIKey, billingModel, upstreamModel string, req GrokMediaRequestInfo) (*FrozenVideoBillingQuote, error) {
	if key == nil || key.Group == nil || key.Group.VideoModelBillingRules == nil {
		return nil, nil
	}
	id := CanonicalGrokImagineVideoPriceFamily(upstreamModel)
	if id == "" {
		id = CanonicalGrokImagineVideoPriceFamily(billingModel)
	}
	rule, configured := key.Group.VideoModelBillingRules[id]
	// Keep the existing explicit group video and token pricing precedence.
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, key); resolved != nil && resolved.Mode == BillingModeToken {
		return nil, nil
	}
	multiplier := key.Group.RateMultiplier
	if !key.Group.VideoRateIndependent && key.GroupID != nil {
		multiplier = s.ResolveUserGroupRateMultiplier(ctx, key.UserID, *key.GroupID, multiplier)
	}
	multiplier = resolveVideoRateMultiplier(key, multiplier)
	result := &OpenAIForwardResult{VideoResolution: req.Resolution, VideoDurationSeconds: req.DurationSeconds, UnifiedVideoBilling: true}
	cost, err := s.calculateOpenAIVideoCostWithRule(ctx, billingModel, key, result, multiplier, rule, configured)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(cost.ActualCost) || math.IsInf(cost.ActualCost, 0) || math.IsNaN(cost.TotalCost) || math.IsInf(cost.TotalCost, 0) {
		return nil, &GrokMediaInputValidationError{Reason: "configured video price exceeds the billing range"}
	}
	return &FrozenVideoBillingQuote{Cost: *cost, Multiplier: multiplier, Seconds: req.DurationSeconds}, nil
}

func cloneVideoBillingRules(in domain.VideoModelBillingRules) domain.VideoModelBillingRules {
	out := maps.Clone(in)
	for id, rule := range out {
		rule.Rates, rule.SpecRates = maps.Clone(rule.Rates), maps.Clone(rule.SpecRates)
		out[id] = rule
	}
	return out
}

// EffectiveVideoBillingRules projects legacy prices for the editor only. The
// runtime uses legacy behavior until the group explicitly saves unified rules.
// A non-nil empty map must not resurrect legacy prices after a deletion.
func (g *Group) EffectiveVideoBillingRules() (domain.VideoModelBillingRules, []string) {
	if g.VideoModelBillingRules != nil {
		return g.VideoModelBillingRules, nil
	}
	out := make(domain.VideoModelBillingRules)
	conflicts := make(map[string]bool)
	for id, rates := range g.VideoModelPrices {
		if profile, registered := falVideoProfiles[id]; registered && profile.SpecPricing {
			conflicts[id] = true
			continue
		}
		out[id] = domain.VideoModelBillingRule{Unit: domain.VideoBillingPerSecond, Rates: rates}
	}
	for id, rates := range g.VideoModelSpecPrices {
		profile, registered := falVideoProfiles[id]
		if (registered && !profile.SpecPricing) || id == VideoPriceFamilyGrokImagineVideo || id == VideoPriceFamilyGrokImagineVideo15 {
			conflicts[id] = true
			continue
		}
		if _, exists := out[id]; exists {
			conflicts[id] = true
			if !registered || !profile.SpecPricing {
				continue
			}
		}
		out[id] = domain.VideoModelBillingRule{Unit: domain.VideoBillingPerGeneration, SpecRates: rates}
	}
	return out, slices.Sorted(maps.Keys(conflicts))
}

func validateVideoBillingRules(rules, previous domain.VideoModelBillingRules) error {
	if rules == nil {
		return nil
	}
	catalog := make(map[string]VideoBillingModel)
	for _, model := range VideoBillingCatalog() {
		catalog[model.ID] = model
	}
	for id, rule := range rules {
		// Allow untouched historical rows and keys to round-trip, not new
		// unsupported values. The admin can still remove an obsolete rule.
		old, existed := previous[id]
		if existed && old.Unit == rule.Unit && maps.Equal(old.Rates, rule.Rates) && maps.Equal(old.SpecRates, rule.SpecRates) {
			continue
		}
		model, ok := catalog[id]
		if !ok || (rule.Unit != domain.VideoBillingPerSecond && rule.Unit != domain.VideoBillingPerGeneration) || len(rule.Rates)+len(rule.SpecRates) == 0 {
			return invalidVideoBillingRule(id, "unknown model, invalid unit, or no rates")
		}
		for key, price := range rule.Rates {
			if oldPrice, ok := old.Rates[key]; ok && old.Unit == rule.Unit && oldPrice == price {
				continue
			}
			if !slices.Contains(model.Resolutions, key) || !validModelVideoRate(price, model.AllowDefault) {
				return invalidVideoBillingRule(id, "invalid resolution or rate: "+key)
			}
		}
		for key, price := range rule.SpecRates {
			if oldPrice, ok := old.SpecRates[key]; ok && old.Unit == rule.Unit && oldPrice == price {
				continue
			}
			if !slices.ContainsFunc(model.Specs, func(option VideoPriceOption) bool { return option.Key == key }) || !validModelVideoRate(price, model.AllowDefault) {
				return invalidVideoBillingRule(id, "invalid output spec or rate: "+key)
			}
		}
	}
	return nil
}

func invalidVideoBillingRule(id, reason string) error {
	return infraerrors.BadRequest("INVALID_VIDEO_BILLING_RULE", fmt.Sprintf("%s: %s", id, reason))
}

func validModelVideoRate(price float64, allowZero bool) bool {
	return positiveVideoQuote(price) || (allowZero && price == 0)
}

func videoRuleRate(rule domain.VideoModelBillingRule, tier, spec string) (float64, bool) {
	if price, ok := rule.SpecRates[spec]; spec != "" && ok {
		return price, true
	}
	// Auto is an exact billing tier, not a wildcard for fixed resolutions.
	price, ok := rule.Rates[tier]
	return price, ok
}

func videoRuleCost(unit string, rate float64, count, seconds int, multiplier float64) *CostBreakdown {
	total := rate * float64(count)
	// Seconds are the normalized video length, never queue or polling latency.
	if unit == domain.VideoBillingPerSecond {
		total *= float64(seconds)
	}
	return &CostBreakdown{TotalCost: total, ActualCost: QuantizeUsageBillingAmount(total * math.Max(0, multiplier)), BillingMode: string(BillingModeVideo)}
}
