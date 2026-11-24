package repository

import (
	"context"
	"database/sql"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

func (r *apiKeyRepository) WithDefaultAPIKeyTx(ctx context.Context, userID int64, fn func(context.Context, *service.Group, func() (*service.APIKey, error)) error) error {
	// Read committed gives the query after the lock a fresh snapshot even
	// when the database defaults to a stronger isolation level.
	tx, err := r.client.BeginTx(ctx, &entsql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin default api key transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx = dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	// The lock must precede the lookup, so a concurrent request on another
	// instance sees the committed key rather than generating a second one.
	if _, err := client.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ensure_default_api_key:' || $1::bigint::text, 0))`, userID); err != nil {
		return fmt.Errorf("lock default api key for user: %w", err)
	}

	m, err := findOrCreateDefaultCompositeGroup(ctx, client)
	if err != nil {
		return err
	}
	g := groupEntityToService(m)
	firstKey := func() (*service.APIKey, error) {
		// Do not filter by status or expiry: that could silently bypass an
		// existing key's restrictions by issuing a replacement.
		row, err := client.APIKey.Query().
			Where(apikey.UserIDEQ(userID), apikey.GroupIDEQ(g.ID), apikey.DeletedAtIsNil()).
			Order(apikey.ByID()).First(ctx)
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find first default api key: %w", err)
		}
		key := apiKeyEntityToService(row)
		key.Group = g
		return key, nil
	}
	if err := fn(ctx, g, firstKey); err != nil {
		return err
	}
	return tx.Commit()
}

func findOrCreateDefaultCompositeGroup(ctx context.Context, client *dbent.Client) (*dbent.Group, error) {
	lookup := func() (*dbent.Group, error) {
		// Hold the selected group steady against administrative updates/deletes
		// until key lookup or creation commits, without serializing all users.
		return client.Group.Query().
			Where(group.NameEQ(service.DefaultCompositeGroupName), group.DeletedAtIsNil()).
			ForShare().Only(ctx)
	}
	g, err := lookup()
	if !dbent.IsNotFound(err) {
		return g, err
	}

	// Match the partial unique index for non-deleted names. A competing
	// writer may have inserted a disabled or non-composite group: always
	// re-read the winner so the service validates the actual group.
	_, err = client.ExecContext(ctx, `
		INSERT INTO groups (name, description, platform, status, subscription_type, is_exclusive, rate_multiplier)
		VALUES ($1, 'Default group', $2, $3, $4, FALSE, 1.0)
		ON CONFLICT (name) WHERE deleted_at IS NULL DO NOTHING`,
		service.DefaultCompositeGroupName, service.PlatformComposite, service.StatusActive, service.SubscriptionTypeStandard)
	if err != nil {
		return nil, fmt.Errorf("insert default composite group: %w", err)
	}
	return lookup()
}
