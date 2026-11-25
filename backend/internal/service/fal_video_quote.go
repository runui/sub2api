package service

import (
	"fmt"
	"math"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
)

type VideoEndpointPricing struct {
	Endpoint string             `json:"endpoint"`
	Unit     string             `json:"unit"`
	Options  []VideoPriceOption `json:"options"`
}

type VideoPriceOption struct {
	Key         string `json:"key"`
	Resolution  string `json:"resolution,omitempty"`
	Duration    int    `json:"duration,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// FalVideoPricingCatalog exposes contract-defined keys, not provider costs or
// the current group's prices. The admin editor and runtime use the same keys.
func FalVideoPricingCatalog() []VideoEndpointPricing {
	rows := make([]VideoEndpointPricing, 0, len(falVideoProfiles))
	for id, profile := range falVideoProfiles {
		row := VideoEndpointPricing{Endpoint: id, Unit: "second"}
		if profile.SpecPricing {
			row.Unit = "generation"
			for _, spec := range profile.specs() {
				row.Options = append(row.Options, VideoPriceOption{spec.key(), spec.Resolution, spec.Duration, spec.AspectRatio})
			}
		} else {
			for _, resolution := range profile.Resolutions {
				row.Options = append(row.Options, VideoPriceOption{Key: falVideoResolutionPriceKey(resolution), Resolution: resolution})
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Endpoint < rows[j].Endpoint })
	return rows
}

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
	price := falVideoEndpointPrice(group, profile, input)
	if price != nil && positiveVideoQuote(*price) {
		amount := *price * multiplier
		if !profile.SpecPricing {
			amount *= float64(input.Duration)
		}
		if amount = QuantizeUsageBillingAmount(amount); positiveVideoQuote(amount) {
			return amount, nil
		}
	}
	return 0, fmt.Errorf("%w: configure a positive video price for this endpoint and output spec", ErrVideoTaskInvalid)
}
