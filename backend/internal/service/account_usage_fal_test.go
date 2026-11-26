package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type falUsageTestUpstream struct {
	HTTPUpstream
	do func(*http.Request, string, int64, int) (*http.Response, error)
}

func (u falUsageTestUpstream) Do(r *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	return u.do(r, proxy, id, concurrency)
}

func falUsageResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func testFalUsageAccount(id int64, key string) Account {
	return Account{ID: id, Platform: PlatformFal, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": key}}
}

func TestFalUsageAggregatesAllPages(t *testing.T) {
	account := testFalUsageAccount(21, "sample-key")
	var requests []*http.Request
	s := &AccountUsageService{falUsageUpstream: falUsageTestUpstream{do: func(r *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
		requests = append(requests, r)
		require.Equal(t, "http://proxy.test:8080", proxy)
		require.Equal(t, account.ID, id)
		require.Equal(t, 1, concurrency)
		if len(requests) == 1 {
			return falUsageResponse(200, `{"summary":[{"endpoint_id":"fal-ai/test","unit":"second","currency":"USD","quantity":1.2,"cost_total":0.1,"unit_price":5}],"has_more":true,"next_cursor":"next/+="}`), nil
		}
		return falUsageResponse(200, `{"summary":[{"endpoint_id":"fal-ai/test","unit":"second","currency":"USD","quantity":2.3,"cost_total":0.2},{"endpoint_id":"fal-ai/test","unit":"video","currency":"USD","quantity":1,"cost_total":0.01},{"endpoint_id":"fal-ai/test","unit":"second","currency":"EUR","quantity":2,"cost_total":0.4}],"has_more":false,"next_cursor":null}`), nil
	}}}
	now := time.Date(2026, 11, 26, 12, 30, 0, 789, time.FixedZone("UTC+8", 8*3600))
	result, err := s.fetchFalUsage(context.Background(), &account, "sample-key", "http://proxy.test:8080", now)
	require.NoError(t, err)
	require.Len(t, requests, 2)
	for _, r := range requests {
		require.Equal(t, "https", r.URL.Scheme)
		require.Equal(t, "api.fal.ai", r.URL.Host)
		require.Equal(t, "/v1/models/usage", r.URL.Path)
		require.Equal(t, "Key sample-key", r.Header.Get("Authorization"))
		require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
		require.True(t, HTTPUpstreamPublicHostsOnly(r.Context()))
		q := r.URL.Query()
		require.Equal(t, "summary", q.Get("expand"))
		require.Equal(t, "false", q.Get("bound_to_timeframe"))
		require.Equal(t, "2026-11-19T04:30:00Z", q.Get("start"))
		require.Equal(t, "2026-11-26T04:30:00Z", q.Get("end"))
		require.Empty(t, q.Get("api_key_id"))
	}
	require.Equal(t, "next/+=", requests[1].URL.Query().Get("cursor"))
	require.Equal(t, []FalUsageTotal{{Currency: "EUR", CostTotal: "0.4"}, {Currency: "USD", CostTotal: "0.31"}}, result.Totals)
	require.Len(t, result.Details, 3)
	require.Equal(t, "3.5", result.Details[1].Quantity)
	require.Equal(t, "0.3", result.Details[1].CostTotal)
}

func TestFalUsageRejectsIncompleteResults(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"unauthorized", "sample-key", "FAL_USAGE_INVALID_KEY", 401},
		{"forbidden", "sample-key", "FAL_USAGE_KEY_PERMISSIONS", 403},
		{"limited", "sample-key", "FAL_USAGE_RATE_LIMITED", 429},
		{"missing summary", `{"has_more":false}`, "FAL_USAGE_INVALID_RESPONSE", 200},
		{"missing cost", `{"summary":[{"endpoint_id":"model","unit":"second","currency":"USD","quantity":2}],"has_more":false}`, "FAL_USAGE_INVALID_RESPONSE", 200},
		{"repeated cursor", `{"summary":[],"has_more":true,"next_cursor":"again"}`, "FAL_USAGE_INVALID_RESPONSE", 200},
		{"missing cursor", `{"summary":[],"has_more":true}`, "FAL_USAGE_INVALID_RESPONSE", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := testFalUsageAccount(21, "sample-key")
			calls := 0
			s := &AccountUsageService{falUsageUpstream: falUsageTestUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
				calls++
				if calls == 1 {
					return falUsageResponse(200, `{"summary":[{"endpoint_id":"model","unit":"second","currency":"USD","quantity":1,"cost_total":1}],"has_more":true,"next_cursor":"again"}`), nil
				}
				return falUsageResponse(tc.status, tc.body), nil
			}}}
			result, err := s.fetchFalUsage(context.Background(), &account, "sample-key", "", time.Now())
			require.Nil(t, result)
			require.Equal(t, tc.reason, infraerrors.Reason(err))
			require.NotContains(t, err.Error(), "sample-key")
		})
	}
}

func TestFalUsageReusesAccountFlowAndRestrictsWorkspaceAccess(t *testing.T) {
	ctx := context.Background()
	a := testFalUsageAccount(21, "sample-key")
	b := testFalUsageAccount(22, "rejected-key")
	repo := &stubOpenAIAccountRepo{accounts: []Account{a, b}}
	authorizations := make(chan string, 10)
	s := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), falUsageUpstream: falUsageTestUpstream{do: func(r *http.Request, _ string, id int64, _ int) (*http.Response, error) {
		authorizations <- r.Header.Get("Authorization")
		if id == b.ID {
			return falUsageResponse(403, ""), nil
		}
		return falUsageResponse(200, `{"summary":[],"has_more":false,"next_cursor":null}`), nil
	}}}

	restricted := adminaccess.WithPrincipal(ctx, &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.usage.read"}, AllGroups: true, IncludeUngrouped: true, AccountTypes: []string{AccountTypeAPIKey}, Platforms: []string{PlatformFal}}}}})
	_, err := s.GetUsage(restricted, a.ID)
	require.Equal(t, "FAL_WORKSPACE_USAGE_DENIED", infraerrors.Reason(err))
	require.Empty(t, authorizations)

	first, err := s.GetUsage(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, first.FalUsage.Totals)
	require.Empty(t, first.FalUsage.Details)
	require.Equal(t, "Key sample-key", <-authorizations)
	_, err = s.GetUsage(ctx, a.ID)
	require.NoError(t, err)
	require.Empty(t, authorizations)
	_, err = s.GetUsage(ctx, a.ID, true)
	require.NoError(t, err)
	require.Equal(t, "Key sample-key", <-authorizations)

	repo.accounts[0].Credentials = map[string]any{"api_key": "rotated-key"}
	usage, failures, err := s.GetUsageBatch(ctx, []int64{a.ID, b.ID}, false)
	require.NoError(t, err)
	require.NotNil(t, usage[a.ID].FalUsage)
	require.NotContains(t, usage, b.ID)
	require.Contains(t, failures[b.ID], "lacks workspace usage permissions")
	require.ElementsMatch(t, []string{"Key rotated-key", "Key rejected-key"}, []string{<-authorizations, <-authorizations})
}
