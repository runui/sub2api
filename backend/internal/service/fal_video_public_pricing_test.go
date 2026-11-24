package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type falPricingTestTransport func(*http.Request) (*http.Response, error)

func (f falPricingTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFalVideoPublicPricing(t *testing.T) {
	const intro = "## Pricing\n\nPriced per second of output video, by resolution: "
	const text = intro + "**480p** at **$0.08**/sec, **720p** at **$0.14**/sec, **1080p** at **$0.25**/sec."
	const image = intro + "480p at **$0.08**/sec, 720p at **$0.14**/sec, 1080p at **$0.25**/sec. Each reference image adds **$0.01**."
	for _, tc := range []struct{ name, imageDoc, wantError string }{
		{"matching rates", image, ""},
		{"different rates", strings.Replace(image, "$0.14", "$0.16", 1), "differ for 720p"},
		{"missing tiers", intro + "720p at $0.14/sec.", "does not list all three"},
		{"outside pricing section", "## Examples\n" + strings.TrimPrefix(image, "## Pricing\n"), "does not list all three"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: falPricingTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				require.Empty(t, r.Header.Get("Authorization"))
				endpoint, doc := falVideoTextEndpoint, text
				if calls == 2 {
					endpoint, doc = falVideoImageEndpoint, tc.imageDoc
				}
				require.Equal(t, "https://fal.ai/models/"+endpoint+"/llms.txt", r.URL.String())
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(doc))}, nil
			})}
			rates, err := fetchFalVideoPublicPricing(context.Background(), client)
			require.Equal(t, 2, calls)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, rates)
				return
			}
			require.NoError(t, err)
			require.Equal(t, map[string]float64{"480p": 0.08, "720p": 0.14, "1080p": 0.25}, rates)
		})
	}
}
