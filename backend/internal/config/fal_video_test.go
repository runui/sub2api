package config

import (
	"math"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestFalVideoConfigYAML(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(`
fal_video:
  routes:
    - group_id: 12
      account_id: 34
      model: grok-imagine-video-1.5
      price_usd_per_second:
        "720p": 0.10
`)))
	var cfg Config
	require.NoError(t, v.Unmarshal(&cfg))
	require.NoError(t, cfg.FalVideo.Validate())
	require.Equal(t, []FalVideoRoute{{GroupID: 12, AccountID: 34, Model: "grok-imagine-video-1.5", PriceUSDPerSecond: map[string]float64{"720p": 0.1}}}, cfg.FalVideo.Routes)
}

func TestFalVideoConfigValidation(t *testing.T) {
	valid := FalVideoRoute{GroupID: 1, AccountID: 2, Model: "video", PriceUSDPerSecond: map[string]float64{"720p": 0.1}}
	require.NoError(t, (FalVideoConfig{}).Validate())
	require.ErrorContains(t, (FalVideoConfig{Routes: []FalVideoRoute{valid, valid}}).Validate(), "duplicate")
	valid.PriceUSDPerSecond["1080p"] = math.NaN()
	require.ErrorContains(t, (FalVideoConfig{Routes: []FalVideoRoute{valid}}).Validate(), "price must be")
	delete(valid.PriceUSDPerSecond, "720p")
	require.ErrorContains(t, (FalVideoConfig{Routes: []FalVideoRoute{valid}}).Validate(), "720p price is required")
}
