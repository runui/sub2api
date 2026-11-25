package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountModelCatalogSnapshotRepository = (*accountRepository)(nil)

func (r *accountRepository) GetAccountModelCatalogSnapshot(ctx context.Context, accountID int64, source string) (*service.UpstreamModelCatalog, error) {
	rows, err := clientFromContext(ctx, r.client).QueryContext(ctx, `
		SELECT started_at, catalog FROM account_model_catalogs WHERE account_id = $1 AND source = $2`, accountID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var startedAt time.Time
	var payload []byte
	if err := rows.Scan(&startedAt, &payload); err != nil {
		return nil, err
	}
	var catalog service.UpstreamModelCatalog
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return nil, err
	}
	catalog.ObservedAt = &startedAt
	return &catalog, nil
}

func (r *accountRepository) SaveAccountModelCatalogSnapshot(ctx context.Context, account *service.Account, source string, startedAt time.Time, catalog *service.UpstreamModelCatalog) error {
	payload, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	credentials, err := json.Marshal(normalizeJSONMap(account.Credentials))
	if err != nil {
		return err
	}
	// Verify the connection scope against the account without retaining credentials
	// in the snapshot. The row lock serializes the check with account updates.
	// A slower, older sync must not supersede a newer complete observation.
	_, err = clientFromContext(ctx, r.client).ExecContext(ctx, `
		INSERT INTO account_model_catalogs (account_id, source, started_at, catalog)
		SELECT id, $2, $3, $4::jsonb FROM accounts
		WHERE id = $1 AND deleted_at IS NULL AND platform = $5 AND type = $6
		  AND credentials = $7::jsonb AND proxy_id IS NOT DISTINCT FROM $8
		FOR SHARE
		ON CONFLICT (account_id, source) DO UPDATE SET started_at = EXCLUDED.started_at, catalog = EXCLUDED.catalog
		WHERE account_model_catalogs.started_at < EXCLUDED.started_at`,
		account.ID, source, startedAt, string(payload), account.Platform, account.Type, string(credentials), account.ProxyID)
	return err
}
