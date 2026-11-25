package service

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
)

type falVideoSpec struct {
	Resolution  string
	Duration    int
	AspectRatio string
}

func falVideoSpecFromInput(input falvideo.Input) falVideoSpec {
	return falVideoSpec{input.Resolution, input.Duration, input.AspectRatio}
}

// This key is persisted in group pricing. Change a contract version when
// changing its wire payload; never reuse a spec key for a different output.
func (s falVideoSpec) key() string {
	key := s.Resolution + "-" + strconv.Itoa(s.Duration) + "s"
	if s.AspectRatio != "" {
		key += "-" + strings.ReplaceAll(s.AspectRatio, ":", "x")
	}
	return key
}

func registerFalVideoProfiles(profiles map[string]falVideoProfile) {
	add := func(id string, p falVideoProfile) {
		p.Endpoint, p.ResultPath = id, []string{"video"}
		profiles[id] = p
	}
	pair := func(base string, text, image falVideoProfile, imageField string) {
		add(base+"/text-to-video", text)
		image.ImageField = imageField
		add(base+"/image-to-video", image)
	}
	// Wire fields and enums come from the fal Queue schemas for the exact IDs below.
	// See deploy/FAL_VIDEO_ENDPOINT_CONTRACTS.md for schema and billing references.
	wan := falVideoProfile{Version: "wan26-v1", DurationEncoding: "string", Durations: []int{5, 10, 15}, DefaultDuration: 5,
		Resolutions: []string{"720p", "1080p"}, DefaultResolution: "720p", SendResolution: true, MaxPromptChars: 1500,
		FixedFields: map[string]any{"multi_shots": false}}
	wanText := wan
	wanText.SendAspect, wanText.DefaultAspectRatio = true, "16:9"
	wanText.AspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4"}
	pair("wan/v2.6", wanText, wan, "image_url")
	wan.ImageField, wan.FixedFields = "image_url", map[string]any{"multi_shots": false, "generate_audio": false}
	add("wan/v2.6/image-to-video/flash", wan)

	// Kling chooses output dimensions itself. The auto price tier is not a
	// caller-selectable resolution and does not promise an exact pixel count.
	kling := falVideoProfile{Version: "kling3-v1", DurationEncoding: "string", DefaultDuration: 5,
		Durations: []int{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, Resolutions: []string{""},
		FixedFields: map[string]any{"generate_audio": false}}
	for _, base := range []string{"fal-ai/kling-video/v3/standard", "fal-ai/kling-video/v3/pro", "fal-ai/kling-video/v2.6/pro"} {
		if strings.Contains(base, "/v2.6/") {
			kling.Version, kling.Durations = "kling26-v1", []int{5, 10}
		}
		text := kling
		text.SendAspect, text.DefaultAspectRatio, text.AspectRatios = true, "16:9", []string{"16:9", "9:16", "1:1"}
		pair(base, text, kling, "start_image_url")
	}

	hailuo := falVideoProfile{Version: "hailuo02-v1", Durations: []int{6}, DefaultDuration: 6,
		Resolutions: []string{"1080p"}, DefaultResolution: "1080p"}
	pair("fal-ai/minimax/hailuo-02/pro", hailuo, hailuo, "image_url")

	// Restrict Seedance to a fixed-size, fixed-duration starting contract; its
	// token-based upstream cost is not a general per-second retail price.
	seedance := falVideoProfile{Version: "seedance2-v1", DurationEncoding: "string", Durations: []int{5}, DefaultDuration: 5,
		Resolutions: []string{"720p"}, DefaultResolution: "720p", AspectRatios: []string{"16:9"}, DefaultAspectRatio: "16:9",
		SpecPricing: true, SendResolution: true, SendAspect: true, FixedFields: map[string]any{"generate_audio": false}}
	for _, base := range []string{"bytedance/seedance-2.0", "bytedance/seedance-2.0/fast"} {
		pair(base, seedance, seedance, "image_url")
	}

	pixverse := falVideoProfile{Version: "pixverse55-v1", DurationEncoding: "string", Durations: []int{5, 8, 10}, DefaultDuration: 5,
		Resolutions: []string{"360p", "540p", "720p", "1080p"}, DefaultResolution: "720p", SendResolution: true, SpecPricing: true,
		ExcludedSpecs: []falVideoSpec{{Resolution: "1080p", Duration: 10}}, MaxPromptBytes: 2048,
		FixedFields: map[string]any{"generate_audio_switch": false, "generate_multi_clip_switch": false}}
	pixverseText := pixverse
	pixverseText.SendAspect, pixverseText.DefaultAspectRatio = true, "16:9"
	pixverseText.AspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4"}
	pair("fal-ai/pixverse/v5.5", pixverseText, pixverse, "image_url")

	ltx := falVideoProfile{Version: "ltx23-quality-v1", Durations: []int{5}, DefaultDuration: 5,
		Resolutions: []string{"720p"}, DefaultResolution: "720p", AspectRatios: []string{"16:9"}, DefaultAspectRatio: "16:9",
		SpecPricing: true, FixedFields: map[string]any{"generate_audio": false, "sync_mode": false},
		EncodeSpec: func(body map[string]any, spec falVideoSpec) {
			// Pin the documented 121-frame/24 fps preset and explicit size.
			// LTX image mode otherwise derives dimensions from the source image.
			body["num_frames"], body["frames_per_second"] = spec.Duration*24+1, 24
			body["resolution"] = map[string]int{"width": 1280, "height": 720}
		}}
	pair("fal-ai/ltx-2.3-quality", ltx, ltx, "image_url")
}

func (p falVideoProfile) allowsSpec(s falVideoSpec) bool {
	return !slices.Contains(p.ExcludedSpecs, falVideoSpec{Resolution: s.Resolution, Duration: s.Duration})
}

func (p falVideoProfile) specs() []falVideoSpec {
	aspects := p.AspectRatios
	if len(aspects) == 0 {
		aspects = []string{""}
	}
	var out []falVideoSpec
	for _, resolution := range p.Resolutions {
		for _, duration := range p.Durations {
			for _, aspect := range aspects {
				s := falVideoSpec{resolution, duration, aspect}
				if p.allowsSpec(s) {
					out = append(out, s)
				}
			}
		}
	}
	return out
}
