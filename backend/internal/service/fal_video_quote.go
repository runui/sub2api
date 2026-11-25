package service

import (
	"fmt"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
)

// Keep persisted prices for future or retired specs, but never use one without
// matching the current contract exactly. An unrelated group edit need not know
// about every spec that has ever been published.
func NormalizeVideoModelSpecPrices(in map[string]map[string]float64) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(in))
	for endpoint, prices := range in {
		if endpoint == "" {
			continue
		}
		for spec, price := range prices {
			if spec == "" || !positiveVideoQuote(price) {
				continue
			}
			if out[endpoint] == nil {
				out[endpoint] = make(map[string]float64)
			}
			out[endpoint][spec] = price
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func positiveVideoQuote(price float64) bool { return price > 0 && !math.IsInf(price, 0) }

func falVideoResolutionPriceKey(resolution string) string {
	if resolution == "" {
		return "auto" // Upstream owns the size; this key is not a client input option.
	}
	return resolution
}

func falVideoEndpointPrice(group *Group, profile falVideoProfile, input falvideo.Input) *float64 {
	if profile.Version == falVideoImagine15Version {
		return group.GetVideoPriceForModel(VideoPriceFamilyGrokImagineVideo15, input.Resolution)
	}
	// Only legacy Imagine can fall back to a group-wide resolution price.
	prices, key := group.VideoModelPrices, falVideoResolutionPriceKey(input.Resolution)
	if profile.SpecPricing {
		prices, key = group.VideoModelSpecPrices, falVideoSpecFromInput(input).key()
	}
	if price, ok := prices[profile.Endpoint][key]; ok {
		return &price
	}
	return nil
}

func quoteFalVideo(group *Group, profile falVideoProfile, input falvideo.Input, multiplier float64) (float64, error) {
	id := profile.Endpoint
	if profile.Version == falVideoImagine15Version {
		id = VideoPriceFamilyGrokImagineVideo15
	}
	var price *float64
	unit := domain.VideoBillingPerSecond
	if group.VideoModelBillingRules == nil {
		price = falVideoEndpointPrice(group, profile, input)
		if profile.SpecPricing {
			unit = domain.VideoBillingPerGeneration
		}
	} else if rule, exists := group.VideoModelBillingRules[id]; exists {
		unit = rule.Unit
		if rate, ok := videoRuleRate(rule, falVideoResolutionPriceKey(input.Resolution), falVideoSpecFromInput(input).key()); ok {
			price = &rate
		} else if id == VideoPriceFamilyGrokImagineVideo15 && unit == domain.VideoBillingPerSecond {
			price = group.GetVideoPrice(input.Resolution)
		}
	} else if id == VideoPriceFamilyGrokImagineVideo15 {
		price = group.GetVideoPrice(input.Resolution)
	}
	if price != nil && positiveVideoQuote(*price) {
		if amount := videoRuleCost(unit, *price, 1, input.Duration, multiplier).ActualCost; positiveVideoQuote(amount) {
			return amount, nil
		}
	}
	return 0, fmt.Errorf("%w: configure a positive video price for this endpoint and output spec", ErrVideoTaskInvalid)
}
