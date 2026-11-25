package service

import (
	"math"

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

func quoteFalVideo(group *Group, profile falVideoProfile, input falvideo.Input, multiplier float64) (float64, error) {
	id := profile.Endpoint
	if profile.Version == falVideoImagine15Version {
		id = VideoPriceFamilyGrokImagineVideo15
	}
	unit, rate, ok := exactGroupVideoRate(group, id, falVideoResolutionPriceKey(input.Resolution), falVideoSpecFromInput(input).key(), profile.SpecPricing)
	if ok && positiveVideoQuote(rate) {
		if amount := videoRuleCost(unit, rate, 1, input.Duration, multiplier).ActualCost; positiveVideoQuote(amount) {
			return amount, nil
		}
	}
	return 0, ErrVideoModelPriceMissing
}
