package config

import (
	"fmt"
	"math"
	"strings"
)

type FalVideoConfig struct {
	Routes []FalVideoRoute `mapstructure:"routes"`
}

type FalVideoRoute struct {
	GroupID           int64              `mapstructure:"group_id" json:"group_id"`
	AccountID         int64              `mapstructure:"account_id" json:"account_id"`
	Model             string             `mapstructure:"model" json:"model"`
	PriceUSDPerSecond map[string]float64 `mapstructure:"price_usd_per_second" json:"price_usd_per_second"`
}

func (c FalVideoConfig) Validate() error {
	type routeKey struct {
		group int64
		model string
	}
	seen := make(map[routeKey]bool, len(c.Routes))
	for i, route := range c.Routes {
		fail := func(msg string) error { return fmt.Errorf("fal_video.routes[%d]: %s", i, msg) }
		if route.GroupID <= 0 || route.AccountID <= 0 || route.Model == "" || len(route.Model) > 128 || strings.TrimSpace(route.Model) != route.Model {
			return fail("positive group_id and account_id and a valid model are required")
		}
		key := routeKey{route.GroupID, route.Model}
		if seen[key] {
			return fail("duplicate group_id and model")
		}
		seen[key] = true
		if route.PriceUSDPerSecond["720p"] <= 0 {
			return fail("720p price is required for the default resolution")
		}
		for resolution, rate := range route.PriceUSDPerSecond {
			switch resolution {
			case "480p", "720p", "1080p":
			default:
				return fail("unsupported resolution in price schedule")
			}
			if rate < 0.00000001 || rate > 1000 || math.IsNaN(rate) || math.IsInf(rate, 0) {
				return fail("price must be between 0.00000001 and 1000 USD per second")
			}
		}
	}
	return nil
}
