//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountModelCatalogSnapshotScopeAndRecency(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{
		Name: "fal catalog snapshot", Platform: service.PlatformFal, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "catalog-test-key", "model_mapping": map[string]string{"alias": "legacy"}},
	})
	repo := NewAccountRepository(client, integrationDB, nil).(service.AccountModelCatalogSnapshotRepository)
	const source = "fal_platform_api"
	const otherSource = "secondary_feed"
	now := time.Now().UTC()
	obs := func(model string) *service.UpstreamModelCatalog {
		return &service.UpstreamModelCatalog{Models: []string{model}}
	}
	require.NoError(t, repo.SaveAccountModelCatalogSnapshot(ctx, account, source, now, obs("complete")))
	require.NoError(t, repo.SaveAccountModelCatalogSnapshot(ctx, account, source, now.Add(-time.Second), obs("old")))
	wrongScope := *account
	wrongScope.Credentials = map[string]any{"api_key": "previous-key"}
	require.NoError(t, repo.SaveAccountModelCatalogSnapshot(ctx, &wrongScope, source, now.Add(time.Second), obs("wrong-scope")))
	require.NoError(t, repo.SaveAccountModelCatalogSnapshot(ctx, account, otherSource, now, obs("other")))
	otherAccount := mustCreateAccount(t, client, &service.Account{
		Name: "grok catalog snapshot", Platform: service.PlatformGrok, Type: service.AccountTypeOAuth,
	})
	require.NoError(t, repo.SaveAccountModelCatalogSnapshot(ctx, otherAccount, otherSource, now, obs("grok")))
	readModels := func(accountID int64, source string) []string {
		var raw []byte
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT catalog FROM account_model_catalogs WHERE account_id = $1 AND source = $2`, accountID, source).Scan(&raw))
		var got service.UpstreamModelCatalog
		require.NoError(t, json.Unmarshal(raw, &got))
		return got.Models
	}
	require.Equal(t, []string{"complete"}, readModels(account.ID, source))
	require.Equal(t, []string{"other"}, readModels(account.ID, otherSource))
	require.Equal(t, []string{"grok"}, readModels(otherAccount.ID, otherSource))
	var alias string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT credentials #>> '{model_mapping,alias}' FROM accounts WHERE id = $1`, account.ID).Scan(&alias))
	require.Equal(t, "legacy", alias)
}
