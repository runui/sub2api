package domain

const (
	VideoBillingPerSecond     = "second"
	VideoBillingPerGeneration = "generation"
)

type VideoModelBillingRule struct {
	Unit      string             `json:"unit"`
	Rates     map[string]float64 `json:"rates,omitempty"`
	SpecRates map[string]float64 `json:"spec_rates,omitempty"`
}

// Nil means legacy prices are authoritative; an empty object is an explicit clear.
type VideoModelBillingRules map[string]VideoModelBillingRule
