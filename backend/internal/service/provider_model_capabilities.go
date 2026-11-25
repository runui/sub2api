package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/xai"

func grokNativeVideoModel(model string) bool {
	switch xai.CanonicalImagineVideoModel(model) {
	case xai.DefaultImagineVideoModel, xai.DefaultImagineVideo15Model:
		return true
	}
	return false
}

func discoverGrokCapabilities(models []string, metadata map[string]UpstreamModelMetadata) []UpstreamModelCapability {
	var out []UpstreamModelCapability
	for _, model := range models {
		// The legacy Grok list can contain IDs only: absent evidence is not a chat claim.
		entry := UpstreamModelCapability{ID: model, Source: "grok_catalog", Reason: "unknown_capability", InputModalities: metadata[model].InputModalities}
		switch {
		case grokNativeVideoModel(model):
			entry.Operations = []string{"video.generate"}
			entry.OutputModalities = []string{"video"}
		case isGrokImageGenerationModel(model):
			entry.Operations = []string{"image.generate"}
			entry.OutputModalities = []string{"image"}
		case xai.IsGrokTextResponsesModelID(model):
			entry.Operations = []string{"text.generate"}
			entry.OutputModalities = []string{"text"}
		}
		if len(entry.Operations) > 0 {
			entry.Source, entry.Ready, entry.Reason = "grok_provider_rule", true, ""
		}
		out = append(out, entry)
	}
	return out
}

// Register only operations we actually route through capability matching.
// Discovery may report other operations without enabling their execution.
type accountModelCapability func(*Account, string) bool

var providerModelCapabilities = map[string]map[string]accountModelCapability{
	"video.generate": {
		PlatformGrok: func(account *Account, model string) bool {
			if enabled, set := grokMediaEligibilityOverride(account.Extra); set && !enabled {
				return false
			}
			return account.SupportsNativeVideoModel(model)
		},
		PlatformFal: func(account *Account, model string) bool {
			_, ok := falVideoAccountModel(account, model)
			return ok
		},
	},
}
