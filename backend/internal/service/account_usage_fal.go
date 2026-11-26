package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

const (
	falUsageURL       = "https://api.fal.ai/v1/models/usage"
	falUsageWindow    = 7 * 24 * time.Hour
	falUsageCacheTTL  = 5 * time.Minute
	falUsageTimeout   = 15 * time.Second
	falUsagePageLimit = 20
	falUsageBodyLimit = 1 << 20
)

type FalUsageSummary struct {
	Start   time.Time        `json:"start"`
	End     time.Time        `json:"end"`
	Totals  []FalUsageTotal  `json:"totals"`
	Details []FalUsageDetail `json:"details"`
}

type FalUsageTotal struct {
	Currency  string `json:"currency"`
	CostTotal string `json:"cost_total"`
}

type FalUsageDetail struct {
	FalUsageTotal
	EndpointID string `json:"endpoint_id"`
	Unit       string `json:"unit"`
	Quantity   string `json:"quantity"`
}

type falUsageCacheEntry struct {
	identity [sha256.Size]byte
	usage    *UsageInfo
}

func falWorkspaceUsageDenied() error {
	return infraerrors.New(http.StatusForbidden, "FAL_WORKSPACE_USAGE_DENIED", "fal workspace usage requires unrestricted admin access")
}

func (s *AccountUsageService) getFalUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if s.falUsageUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "FAL_USAGE_NOT_CONFIGURED", "fal usage is not configured")
	}
	if !hasFalVideoAPIKey(account.Credentials) {
		return nil, infraerrors.New(http.StatusBadRequest, "FAL_USAGE_NO_KEY", "No valid fal API key is configured")
	}
	key := account.GetCredential("api_key")
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	// A credential or proxy change must not serve another workspace's cached usage.
	identity := sha256.Sum256([]byte(key + "\x00" + proxy))
	load := func() *falUsageCacheEntry {
		value, _ := s.cache.falUsageCache.Load(account.ID)
		entry, _ := value.(*falUsageCacheEntry)
		if entry != nil && entry.identity == identity {
			return entry
		}
		return nil
	}
	if !force {
		if entry := load(); entry != nil && time.Since(*entry.usage.UpdatedAt) < falUsageCacheTTL {
			return entry.usage, nil
		}
	} else if entry := load(); entry != nil {
		s.cache.falUsageCache.CompareAndDelete(account.ID, entry)
	}

	flightKey := fmt.Sprintf("%d:%x", account.ID, identity)
	result := s.cache.falUsageFlight.DoChan(flightKey, func() (any, error) {
		if !force {
			if entry := load(); entry != nil && time.Since(*entry.usage.UpdatedAt) < falUsageCacheTTL {
				return entry.usage, nil
			}
		}
		// A canceled waiter should not cancel a shared fetch for other account-page requests.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), falUsageTimeout)
		defer cancel()
		summary, err := s.fetchFalUsage(fetchCtx, account, key, proxy, time.Now())
		if err != nil {
			// Billing permission failures must not change the account's inference status.
			return nil, err
		}
		now := time.Now().UTC()
		usage := &UsageInfo{Source: "active", UpdatedAt: &now, FalUsage: summary}
		s.cache.falUsageCache.Store(account.ID, &falUsageCacheEntry{identity: identity, usage: usage})
		return usage, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-result:
		if response.Err != nil {
			return nil, response.Err
		}
		return response.Val.(*UsageInfo), nil
	}
}

type falUsagePage struct {
	Summary *[]struct {
		EndpointID string           `json:"endpoint_id"`
		Unit       string           `json:"unit"`
		Currency   string           `json:"currency"`
		Quantity   *decimal.Decimal `json:"quantity"`
		CostTotal  *decimal.Decimal `json:"cost_total"`
	} `json:"summary"`
	HasMore    *bool  `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

func invalidFalUsage() error {
	return infraerrors.New(http.StatusBadGateway, "FAL_USAGE_INVALID_RESPONSE", "fal returned incomplete or invalid usage data")
}

func validFalUsageAmount(value *decimal.Decimal) bool {
	// Refuse pathological exponents before decimal addition expands them.
	return value != nil && !value.IsNegative() && value.Exponent() >= -100 && value.Exponent() <= 100
}

func (s *AccountUsageService) fetchFalUsage(ctx context.Context, account *Account, key, proxy string, now time.Time) (*FalUsageSummary, error) {
	end := now.UTC().Truncate(time.Second)
	start := end.Add(-falUsageWindow)
	// Fix the exact rolling window once so every cursor refers to the same aggregate.
	query := url.Values{"expand": {"summary"}, "bound_to_timeframe": {"false"}, "start": {start.Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}}
	ctx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(ctx))
	type detailKey struct{ endpoint, unit, currency string }
	type amount struct{ quantity, cost decimal.Decimal }
	details := make(map[detailKey]amount)
	totals := make(map[string]decimal.Decimal)
	seen := make(map[string]bool)
	for i := 0; i < falUsagePageLimit; i++ {
		page, err := s.fetchFalUsagePage(ctx, account, key, proxy, query)
		if err != nil {
			return nil, err
		}
		for _, row := range *page.Summary {
			if strings.TrimSpace(row.EndpointID) == "" || strings.TrimSpace(row.Unit) == "" || len(row.Currency) != 3 || strings.Trim(row.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || !validFalUsageAmount(row.Quantity) || !validFalUsageAmount(row.CostTotal) {
				return nil, invalidFalUsage()
			}
			id := detailKey{row.EndpointID, row.Unit, row.Currency}
			previous := details[id]
			details[id] = amount{previous.quantity.Add(*row.Quantity), previous.cost.Add(*row.CostTotal)}
			totals[row.Currency] = totals[row.Currency].Add(*row.CostTotal)
		}
		if !*page.HasMore {
			summary := &FalUsageSummary{Start: start, End: end, Totals: make([]FalUsageTotal, 0, len(totals)), Details: make([]FalUsageDetail, 0, len(details))}
			for currency, cost := range totals {
				summary.Totals = append(summary.Totals, FalUsageTotal{Currency: currency, CostTotal: cost.String()})
			}
			for id, value := range details {
				summary.Details = append(summary.Details, FalUsageDetail{FalUsageTotal: FalUsageTotal{Currency: id.currency, CostTotal: value.cost.String()}, EndpointID: id.endpoint, Unit: id.unit, Quantity: value.quantity.String()})
			}
			sort.Slice(summary.Totals, func(i, j int) bool { return summary.Totals[i].Currency < summary.Totals[j].Currency })
			sort.Slice(summary.Details, func(i, j int) bool {
				a, b := summary.Details[i], summary.Details[j]
				if a.EndpointID != b.EndpointID {
					return a.EndpointID < b.EndpointID
				}
				if a.Unit != b.Unit {
					return a.Unit < b.Unit
				}
				return a.Currency < b.Currency
			})
			return summary, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil, invalidFalUsage()
		}
		seen[page.NextCursor] = true
		query.Set("cursor", page.NextCursor)
	}
	// Never present an incomplete set of pages as the workspace total.
	return nil, invalidFalUsage()
}

func (s *AccountUsageService) fetchFalUsagePage(ctx context.Context, account *Account, key, proxy string, query url.Values) (*falUsagePage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, falUsageURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, invalidFalUsage()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Key "+key)
	resp, err := s.falUsageUpstream.Do(req, proxy, account.ID, max(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "FAL_USAGE_UNAVAILABLE", "Could not reach the fal usage API")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, infraerrors.New(http.StatusBadGateway, "FAL_USAGE_INVALID_KEY", "fal rejected the configured API key (401)")
	case http.StatusForbidden:
		return nil, infraerrors.New(http.StatusForbidden, "FAL_USAGE_KEY_PERMISSIONS", "The configured fal API key lacks workspace usage permissions (403)")
	case http.StatusTooManyRequests:
		return nil, infraerrors.New(http.StatusTooManyRequests, "FAL_USAGE_RATE_LIMITED", "fal rate limited the usage query (429); try again later")
	default:
		return nil, infraerrors.New(http.StatusBadGateway, "FAL_USAGE_UPSTREAM_ERROR", "fal usage query failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, falUsageBodyLimit+1))
	if err != nil || len(body) > falUsageBodyLimit {
		return nil, invalidFalUsage()
	}
	var page falUsagePage
	if err := json.Unmarshal(body, &page); err != nil || page.Summary == nil || page.HasMore == nil {
		return nil, invalidFalUsage()
	}
	return &page, nil
}
