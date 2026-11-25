package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideo"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	falVideoVeo31            = "fal-ai/veo3.1"
	falVideoVeo31Fast        = "fal-ai/veo3.1/fast"
	falVideoImagine15Version = "imagine15-v1"
)

type falVideoProfile struct {
	Endpoint, Version, ImageField string
	Durations                     []int
	DefaultDuration               int
	DurationSuffix                string
	Resolutions, AspectRatios     []string
	DefaultResolution             string
	DefaultAspectRatio            string
	MaxPromptChars                int
	GenerateAudio                 bool
	ResultPath                    []string
}

// Profiles are reviewed contracts, not inferred from endpoint names. Another
// endpoint can reuse a profile only after its schema and billing are verified.
var falVideoProfiles = func() map[string]falVideoProfile {
	profiles := make(map[string]falVideoProfile)
	for _, base := range []string{falVideoVeo31, falVideoVeo31Fast} {
		profile := falVideoProfile{
			Endpoint: base, Version: "veo31-v1", Durations: []int{4, 6, 8}, DefaultDuration: 8, DurationSuffix: "s",
			Resolutions: []string{"720p", "1080p", "4k"}, DefaultResolution: "720p",
			AspectRatios: []string{"16:9", "9:16"}, DefaultAspectRatio: "16:9", MaxPromptChars: 20000, ResultPath: []string{"video"},
		}
		profiles[base] = profile
		profile.Endpoint, profile.ImageField = base+"/image-to-video", "image_url"
		profile.AspectRatios = []string{"auto", "16:9", "9:16"}
		profile.DefaultAspectRatio = "auto"
		profiles[profile.Endpoint] = profile
	}
	return profiles
}()

func falVideoRegisteredModel(model string) bool {
	_, exists := falVideoProfiles[model]
	return exists || model == falVideoUpstreamModel
}

func falVideoEndpointPrice(group *Group, profile falVideoProfile, resolution string) *float64 {
	if profile.Version == falVideoImagine15Version {
		return group.GetVideoPriceForModel(VideoPriceFamilyGrokImagineVideo15, resolution)
	}
	// A missing tier must not fall back to the Grok group price or a lower resolution.
	if price, ok := group.VideoModelPrices[profile.Endpoint][resolution]; ok {
		return &price
	}
	return nil
}

func prepareFalVideo(model string, req VideoTaskRequest) (falVideoProfile, falvideo.Input, json.RawMessage, error) {
	if model == falVideoUpstreamModel {
		input, err := falVideoInput(req)
		endpoint := falVideoTextEndpoint
		if input.ImageURL != "" {
			endpoint = falVideoImageEndpoint
		}
		profile := falVideoProfile{Endpoint: endpoint, Version: falVideoImagine15Version, ResultPath: []string{"video"}}
		if err != nil {
			return profile, input, nil, err
		}
		body, err := json.Marshal(input)
		return profile, input, body, err
	}
	profile, ok := falVideoProfiles[model]
	if !ok {
		return profile, falvideo.Input{}, nil, ErrVideoTaskNotFound
	}
	canonical, err := validateVideoTaskInput(req)
	input := falvideo.Input(canonical)
	if err != nil {
		return profile, input, nil, err
	}
	if (profile.ImageField != "") != (input.ImageURL != "") {
		return profile, input, nil, fmt.Errorf("%w: image input does not match this model", ErrVideoTaskInvalid)
	}
	if input.ImageURL != "" {
		u, _ := url.Parse(input.ImageURL) // Already parsed by shared validation.
		if strings.ContainsAny(input.ImageURL, " \\#") || urlvalidator.IsBlockedHost(strings.TrimSuffix(u.Hostname(), ".")) {
			return profile, input, nil, fmt.Errorf("%w: image must be a public HTTPS URL", ErrVideoTaskInvalid)
		}
	}
	if profile.MaxPromptChars > 0 && utf8.RuneCountInString(input.Prompt) > profile.MaxPromptChars {
		return profile, input, nil, fmt.Errorf("%w: prompt is too long for this model", ErrVideoTaskInvalid)
	}
	if input.Duration == 0 {
		input.Duration = profile.DefaultDuration
	}
	if !slices.Contains(profile.Durations, input.Duration) {
		return profile, input, nil, fmt.Errorf("%w: duration must be one of %v", ErrVideoTaskInvalid, profile.Durations)
	}
	if input.Resolution == "" {
		input.Resolution = profile.DefaultResolution
	}
	if !slices.Contains(profile.Resolutions, input.Resolution) {
		return profile, input, nil, fmt.Errorf("%w: resolution must be one of %v", ErrVideoTaskInvalid, profile.Resolutions)
	}
	if input.AspectRatio == "" {
		input.AspectRatio = profile.DefaultAspectRatio
	}
	if !slices.Contains(profile.AspectRatios, input.AspectRatio) {
		return profile, input, nil, fmt.Errorf("%w: unsupported aspect_ratio", ErrVideoTaskInvalid)
	}
	// fal charges separately for audio. Pin this rather than relying on a mutable upstream default.
	payload := map[string]any{"prompt": input.Prompt, "resolution": input.Resolution, "generate_audio": profile.GenerateAudio}
	if profile.DurationSuffix != "" {
		payload["duration"] = strconv.Itoa(input.Duration) + profile.DurationSuffix
	} else {
		payload["duration"] = input.Duration
	}
	if input.AspectRatio != "" {
		payload["aspect_ratio"] = input.AspectRatio
	}
	if profile.ImageField != "" {
		payload[profile.ImageField] = input.ImageURL
	}
	body, err := json.Marshal(payload)
	return profile, input, body, err
}

func falVideoResultAt(body json.RawMessage, path []string) (falvideo.Result, error) {
	for _, key := range path {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			return falvideo.Result{}, err
		}
		body = object[key]
	}
	var result falvideo.Result
	if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if strings.TrimSpace(result.URL) == "" || result.Duration < 0 || result.Width < 0 || result.Height < 0 {
		return result, fmt.Errorf("invalid video output")
	}
	return result, nil
}
