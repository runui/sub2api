package service

import (
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

type VideoBillingModel struct {
	ID             string                      `json:"id"`
	Name           string                      `json:"name"`
	Aliases        []string                    `json:"aliases,omitempty"`
	Resolutions    []string                    `json:"resolutions"`
	Specs          []VideoPriceOption          `json:"specs,omitempty"`
	AllowZeroRate  bool                        `json:"allow_zero_rate"`
	Recommendation *VideoBillingRecommendation `json:"recommendation,omitempty"`
}

type VideoPriceOption struct {
	Key         string `json:"key"`
	Resolution  string `json:"resolution"`
	Duration    int    `json:"duration"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

type VideoBillingRecommendation struct {
	domain.VideoModelBillingRule
	Source     string `json:"source"`
	CheckedOn  string `json:"checked_on"`
	Conditions string `json:"conditions,omitempty"`
	Estimated  bool   `json:"estimated,omitempty"`
}

func VideoBillingCatalog() []VideoBillingModel {
	rows := []VideoBillingModel{
		{ID: VideoPriceFamilyGrokImagineVideo, Name: "Grok Imagine Video", Resolutions: falImagineVideoResolutions, AllowZeroRate: true,
			Recommendation: &VideoBillingRecommendation{VideoModelBillingRule: domain.VideoModelBillingRule{Unit: domain.VideoBillingPerSecond, Rates: map[string]float64{"480p": .05, "720p": .07}}, Source: "https://docs.x.ai/developers/models/grok-imagine-video", CheckedOn: "2026-11-25", Conditions: "xai_output_only"}},
		{ID: VideoPriceFamilyGrokImagineVideo15, Name: "Grok Imagine Video 1.5", Aliases: []string{falVideoUpstreamModel, falVideoTextEndpoint, falVideoImageEndpoint}, Resolutions: falImagineVideoResolutions, AllowZeroRate: true,
			Recommendation: &VideoBillingRecommendation{VideoModelBillingRule: domain.VideoModelBillingRule{Unit: domain.VideoBillingPerSecond, Rates: map[string]float64{"480p": .08, "720p": .14, "1080p": .25}}, Source: "https://fal.ai/models/" + falVideoImageEndpoint + "/llms.txt", CheckedOn: "2026-11-25", Conditions: "output_only"}},
	}
	for id, p := range falVideoProfiles {
		row := VideoBillingModel{ID: id, Name: strings.TrimPrefix(id, "fal-ai/")}
		for _, resolution := range p.Resolutions {
			row.Resolutions = append(row.Resolutions, falVideoResolutionPriceKey(resolution))
		}
		for _, spec := range p.specs() {
			row.Specs = append(row.Specs, VideoPriceOption{spec.key(), falVideoResolutionPriceKey(spec.Resolution), spec.Duration, spec.AspectRatio})
		}
		row.Recommendation = recommendVideoRate(row)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// These are official cost references for our pinned request options, not
// retail defaults. Missing or ambiguous upstream prices are intentionally absent.
func recommendVideoRate(model VideoBillingModel) *VideoBillingRecommendation {
	id := model.ID
	r := &VideoBillingRecommendation{VideoModelBillingRule: domain.VideoModelBillingRule{Unit: domain.VideoBillingPerSecond}, Source: "https://fal.ai/models/" + id + "/llms.txt", CheckedOn: "2026-11-25"}
	switch {
	case strings.HasPrefix(id, "fal-ai/veo3.1/fast"):
		r.Rates, r.Conditions = map[string]float64{"720p": .10, "1080p": .10, "4k": .30}, "audio_off"
	case strings.HasPrefix(id, "fal-ai/veo3.1"):
		r.Rates, r.Conditions = map[string]float64{"720p": .20, "1080p": .20, "4k": .40}, "audio_off"
	case id == "wan/v2.6/image-to-video/flash":
		r.Rates, r.Conditions = map[string]float64{"720p": .05, "1080p": .075}, "audio_off"
	case strings.HasPrefix(id, "wan/v2.6/"):
		r.Rates = map[string]float64{"720p": .10, "1080p": .15}
	case strings.HasPrefix(id, "fal-ai/kling-video/v3/standard/"):
		r.Rates, r.Conditions = map[string]float64{"auto": .084}, "audio_off"
	case strings.HasPrefix(id, "fal-ai/kling-video/v3/pro/"):
		r.Rates, r.Conditions = map[string]float64{"auto": .112}, "audio_off"
	case strings.HasPrefix(id, "fal-ai/kling-video/v2.6/pro/"):
		r.Rates, r.Conditions = map[string]float64{"auto": .07}, "audio_off"
	case strings.HasPrefix(id, "fal-ai/minimax/hailuo-02/pro/"):
		r.Rates = map[string]float64{"1080p": .08}
	case strings.HasPrefix(id, "fal-ai/pixverse/v5.5/"):
		r.Unit, r.Conditions = domain.VideoBillingPerGeneration, "single_clip_audio_off"
		r.SpecRates = make(map[string]float64)
		base := map[string]int{"360p": 15, "540p": 15, "720p": 20, "1080p": 40}
		factor := map[int]int{5: 10, 8: 20, 10: 22}
		for _, spec := range model.Specs {
			r.SpecRates[spec.Key] = float64(base[spec.Resolution]*factor[spec.Duration]) / 1000
		}
	case strings.HasPrefix(id, "bytedance/seedance-2.0/"):
		// The published 720p rates are examples of pixel-dependent token pricing.
		// Offer the estimate only for our pinned 5s/16:9 request, not all 720p output.
		r.Unit, r.Estimated = domain.VideoBillingPerGeneration, true
		price := 1.517
		if strings.HasPrefix(id, "bytedance/seedance-2.0/fast/") {
			price = 1.2095
		}
		r.SpecRates = map[string]float64{"720p-5s-16x9": price}
	case strings.HasPrefix(id, "fal-ai/ltx-2.3-quality/"):
		// The official 1280x720/121-frame example matches our pinned payload.
		r.Unit, r.Estimated = domain.VideoBillingPerGeneration, true
		r.SpecRates = map[string]float64{"720p-5s-16x9": .270}
	default:
		return nil
	}
	return r
}
