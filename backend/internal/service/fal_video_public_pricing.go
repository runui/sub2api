package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const falPricingDocLimit = 256 << 10

var falPricingClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var falResolutionRate = regexp.MustCompile(`\b(480p|720p|1080p) at \$([0-9]+(?:\.[0-9]+)?)/sec\b`)

// FetchFalVideoPublicPricing imports the public output rates, not account-specific
// discounts or add-on charges. The pricing API does not expose resolution tiers.
func FetchFalVideoPublicPricing(ctx context.Context) (map[string]float64, error) {
	return fetchFalVideoPublicPricing(ctx, falPricingClient)
}

func fetchFalVideoPublicPricing(ctx context.Context, client *http.Client) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var rates map[string]float64
	for _, endpoint := range []string{falVideoTextEndpoint, falVideoImageEndpoint} {
		docURL := "https://fal.ai/models/" + endpoint + "/llms.txt"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, falPricingDocLimit+1))
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK || len(body) > falPricingDocLimit {
			return nil, fmt.Errorf("unable to read fal pricing documentation for %s", endpoint)
		}
		quote, err := parseFalVideoPublicPricing(string(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", endpoint, err)
		}
		if rates == nil {
			rates = quote
			continue
		}
		for resolution, rate := range quote {
			if rates[resolution] != rate {
				return nil, fmt.Errorf("fal text and image output rates differ for %s", resolution)
			}
		}
	}
	return rates, nil
}

func parseFalVideoPublicPricing(doc string) (map[string]float64, error) {
	rates := make(map[string]float64)
	inPricing := false
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if !inPricing {
			inPricing = line == "## Pricing"
			continue
		}
		if strings.HasPrefix(line, "#") {
			break
		}
		line = strings.ReplaceAll(line, "**", "")
		if !strings.HasPrefix(line, "Priced per second of output video, by resolution:") {
			continue
		}
		for _, match := range falResolutionRate.FindAllStringSubmatch(line, -1) {
			resolution := match[1]
			if _, exists := rates[resolution]; exists {
				return nil, fmt.Errorf("duplicate fal rate for %s", resolution)
			}
			rate, err := strconv.ParseFloat(match[2], 64)
			if err != nil {
				return nil, err
			}
			rates[resolution] = rate
		}
	}
	if len(rates) != 3 {
		return nil, fmt.Errorf("fal pricing documentation does not list all three per-second rates")
	}
	check := config.FalVideoConfig{Routes: []config.FalVideoRoute{{GroupID: 1, AccountID: 1, Model: "video", PriceUSDPerSecond: rates}}}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	return rates, nil
}
