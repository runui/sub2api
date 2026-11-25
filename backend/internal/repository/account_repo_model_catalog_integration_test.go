//go:build integration

package repository

import (
	"context"
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
	repo := NewAccountRepository(client, integrationDB, nil).(*accountRepository)
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
		got, err := repo.GetAccountModelCatalogSnapshot(ctx, accountID, source)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.WithinDuration(t, now, *got.ObservedAt, time.Microsecond)
		return got.Models
	}
	missing, err := repo.GetAccountModelCatalogSnapshot(ctx, account.ID, "missing")
	require.NoError(t, err)
	require.Nil(t, missing)
	require.Equal(t, []string{"complete"}, readModels(account.ID, source))
	require.Equal(t, []string{"other"}, readModels(account.ID, otherSource))
	require.Equal(t, []string{"grok"}, readModels(otherAccount.ID, otherSource))
	var alias string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT credentials #>> '{model_mapping,alias}' FROM accounts WHERE id = $1`, account.ID).Scan(&alias))
	require.Equal(t, "legacy", alias)
}
