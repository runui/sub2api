package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func buildFalVideoCatalogRequest(ctx context.Context, account *Account, cursor string) (*http.Request, error) {
	if account.Type != AccountTypeAPIKey {
		return nil, newUpstreamModelSyncUnsupportedError("fal model sync requires an API key account", nil)
	}
	key := strings.TrimSpace(account.GetCredential("api_key"))
	if key == "" {
		return nil, newUpstreamModelSyncConfigError("No fal API key is available", nil)
	}
	// fal's trending browse stops at a ranked subset. Recent sorts the full set.
	query := url.Values{"limit": {"100"}, "sort": {"recent"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	ctx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(ctx))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.fal.ai/v1/models?"+query.Encode(), nil)
	if err != nil {
		return nil, newUpstreamModelSyncInternalError("Failed to build fal model catalog request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Key "+key)
	return req, nil
}

type falCatalogPage struct {
	Models []struct {
		EndpointID string `json:"endpoint_id"`
		Metadata   struct {
			Status   string `json:"status"`
			Category string `json:"category"`
			Kind     string `json:"kind"`
		} `json:"metadata"`
	} `json:"models"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// These exclusions were checked against the endpoint's required Queue OpenAPI
// inputs. Do not infer a required tail frame or reference array from its name.
var falVideoInputExclusions = map[string]string{
	"fal-ai/veo3.1/first-last-frame-to-video":      "requires_tail_frame",
	"fal-ai/veo3.1/fast/first-last-frame-to-video": "requires_tail_frame",
	"fal-ai/veo3.1/reference-to-video":             "requires_reference_images",
}

func falCatalogCapability(category, kind string) (operation string, inputs, outputs []string, reason string) {
	if kind == "training" || category == "training" {
		return "", nil, nil, "training_endpoint"
	}
	input, output, pair := strings.Cut(category, "-to-")
	known := func(m string) bool {
		switch m {
		case "text", "image", "video", "audio", "speech", "3d", "json":
			return true
		}
		return false
	}
	if pair && known(input) && known(output) {
		if input == "speech" {
			input = "audio"
		}
		if output == "speech" {
			output = "audio"
		}
		reason = "not_adapted"
		if output != "video" {
			reason = "operation_not_supported"
		}
		if input == "audio" || input == "video" || input == "3d" {
			if output == "video" {
				reason = "unsupported_video_input"
			}
		}
		return output + ".generate", []string{input}, []string{output}, reason
	}
	switch {
	case category == "video-editing", category == "lip-sync":
		return "video.generate", []string{"video"}, []string{"video"}, "unsupported_video_input"
	case category == "image-editing":
		return "image.generate", []string{"image"}, []string{"image"}, "operation_not_supported"
	case category == "text-to-music":
		return "audio.generate", []string{"text"}, []string{"audio"}, "operation_not_supported"
	case category == "llm":
		// The LLM category does not establish a chat protocol or input modalities.
		return "text.generate", nil, []string{"text"}, "operation_not_supported"
	}
	return "", nil, nil, "unknown_capability"
}

func (s *AccountTestService) readFalCatalogPage(req *http.Request, account *Account) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := s.readUpstreamModelsResponse(req, account)
		if attempt == 5 || upstreamModelSyncStatusCode(err) != http.StatusTooManyRequests {
			return body, err
		}
		// Only retry an idempotent catalog read, never an inference submission.
		timer := time.NewTimer(time.Second << attempt)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}

func (s *AccountTestService) discoverFalModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, falVideoHTTPTimeout*4)
	defer cancel()
	entries := make(map[string]UpstreamModelCapability)
	seenCursors := make(map[string]bool)
	cursor := ""
	// Build locally until the last page succeeds. A partial listing is not a
	// reliable basis for replacing the visible catalog or adding model mappings.
	for page := 0; page < 100; page++ {
		req, err := buildFalVideoCatalogRequest(ctx, account, cursor)
		if err != nil {
			return nil, err
		}
		body, err := s.readFalCatalogPage(req, account)
		if err != nil {
			return nil, err
		}
		var result falCatalogPage
		if err = json.Unmarshal(body, &result); err != nil || result.Models == nil {
			return nil, newUpstreamModelSyncUpstreamError("Invalid fal model catalog response", err)
		}
		for _, model := range result.Models {
			id := strings.TrimSpace(model.EndpointID)
			if id == "" {
				continue
			}
			operation, inputs, outputs, reason := falCatalogCapability(model.Metadata.Category, model.Metadata.Kind)
			entry := UpstreamModelCapability{ID: id, Category: model.Metadata.Category, Source: "fal_catalog", Status: model.Metadata.Status, Reason: reason, InputModalities: inputs, OutputModalities: outputs}
			if operation != "" {
				entry.Operations = []string{operation}
			}
			if model.Metadata.Status != "active" {
				entry.Reason = "not_active"
			}
			if profile, ready := falVideoProfiles[id]; ready && model.Metadata.Status == "active" {
				entry.Operations, entry.Ready, entry.Reason = []string{"video.generate"}, true, ""
				entry.RequiresPrice = true
				entry.InputModalities, entry.OutputModalities = []string{"text"}, []string{"video"}
				if profile.ImageField != "" {
					entry.InputModalities = append(entry.InputModalities, "image")
				}
			}
			if (id == falVideoTextEndpoint || id == falVideoImageEndpoint) && model.Metadata.Status == "active" {
				entry.Reason = "legacy_family_only"
			}
			if reason := falVideoInputExclusions[id]; reason != "" && model.Metadata.Status == "active" {
				entry.Reason = reason
			}
			entries[id] = entry
		}
		if !result.HasMore {
			if result.NextCursor != "" || (page > 0 && len(result.Models) == 0) {
				return nil, newUpstreamModelSyncUpstreamError("Invalid fal model catalog response", errors.New("inconsistent final page"))
			}
			return finishFalModelCatalog(entries), nil
		}
		if result.NextCursor == "" || seenCursors[result.NextCursor] {
			return nil, newUpstreamModelSyncUpstreamError("Invalid fal model catalog response", errors.New("missing or repeated cursor"))
		}
		cursor = result.NextCursor
		seenCursors[cursor] = true
	}
	return nil, newUpstreamModelSyncUpstreamError("Fal model catalog exceeds the pagination limit", nil)
}

func finishFalModelCatalog(entries map[string]UpstreamModelCapability) *UpstreamModelCatalog {
	if entries[falVideoTextEndpoint].Status == "active" && entries[falVideoImageEndpoint].Status == "active" {
		entries[falVideoUpstreamModel] = UpstreamModelCapability{ID: falVideoUpstreamModel, Operations: []string{"video.generate"}, InputModalities: []string{"text", "image"}, OutputModalities: []string{"video"}, Status: "active", Source: "gateway_profile", Ready: true}
	}
	result := &UpstreamModelCatalog{Models: []string{}, DiscoverySource: "upstream"}
	for _, entry := range entries {
		result.Capabilities = append(result.Capabilities, entry)
		if entry.Ready {
			result.Models = append(result.Models, entry.ID)
		}
	}
	sort.Strings(result.Models)
	sort.Slice(result.Capabilities, func(i, j int) bool { return result.Capabilities[i].ID < result.Capabilities[j].ID })
	return result
}
