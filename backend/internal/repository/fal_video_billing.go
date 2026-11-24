package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/Wei-Shaw/sub2api/internal/pkg/falvideotask"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type FalVideoBillingRepository struct {
	usageBilling usageBillingRepository
}

func NewFalVideoBillingRepository(db *sql.DB) *FalVideoBillingRepository {
	return &FalVideoBillingRepository{usageBilling: usageBillingRepository{db: db}}
}

type falVideoBillingTask struct {
	state, billingState       string
	userID, apiKeyID, groupID int64
	duration                  int
	unitPriceUSD              float64
}

func (t falVideoBillingTask) amount() (float64, error) {
	amount := service.QuantizeUsageBillingAmount(float64(t.duration) * t.unitPriceUSD)
	if t.duration <= 0 || amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, errors.New("invalid fal video billing amount")
	}
	return amount, nil
}

// Reserve freezes the full requested cost before upstream submission.
func (r *FalVideoBillingRepository) Reserve(ctx context.Context, t falvideotask.Task) error {
	return r.withLockedTask(ctx, t.ID, func(tx *sql.Tx, task falVideoBillingTask) error {
		if task.state != "submitting" {
			return falvideotask.ErrInvalidTransition
		}
		switch task.billingState {
		case "reserved":
			return nil
		case "unreserved":
		default:
			return falvideotask.ErrInvalidTransition
		}

		amount, err := task.amount()
		if err != nil {
			return err
		}
		_, err = reserveUsageBillingBatchImageBalance(ctx, tx, &service.BatchImageBalanceHoldCommand{
			UserID: task.userID, HoldAmount: amount,
		})
		if err != nil {
			return err
		}
		if err := checkFalVideoPlatformQuota(ctx, tx, task.userID, task.groupID); err != nil {
			return err
		}
		if err := checkFalVideoBillingAPIKey(ctx, tx, t.ID, task.apiKeyID, amount); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE fal_video_tasks
			SET billing_state = 'reserved', updated_at = clock_timestamp() WHERE id = $1`, t.ID)
		return err
	})
}

// Settle captures a successful task's reservation or releases it on failure.
func (r *FalVideoBillingRepository) Settle(ctx context.Context, t falvideotask.Task, success bool) error {
	pending, terminal := "pending_failure", "released"
	if success {
		pending, terminal = "pending_success", "captured"
	}
	return r.withLockedTask(ctx, t.ID, func(tx *sql.Tx, task falVideoBillingTask) error {
		if task.billingState == terminal {
			return nil
		}
		if task.state != pending {
			return falvideotask.ErrInvalidTransition
		}
		if task.billingState != "reserved" && !(task.billingState == "unreserved" && !success) {
			return falvideotask.ErrInvalidTransition
		}

		if task.billingState == "reserved" {
			amount, err := task.amount()
			if err != nil {
				return err
			}
			refund := amount
			if success {
				refund = 0
			}
			// Settle existing holds even if the owner has since been soft-deleted.
			res, err := tx.ExecContext(ctx, `UPDATE users
				SET balance = balance + $3, frozen_balance = COALESCE(frozen_balance, 0) - $1, updated_at = NOW()
				WHERE id = $2 AND COALESCE(frozen_balance, 0) >= $1`, amount, task.userID, refund)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return errors.New("fal video frozen balance is insufficient or user is missing")
			}
			if success {
				if err := incrementFalVideoBillingAPIKey(ctx, tx, task.apiKeyID, amount); err != nil {
					return err
				}
			}
		}

		_, err := tx.ExecContext(ctx, `UPDATE fal_video_tasks SET billing_state = $2 WHERE id = $1`, t.ID, terminal)
		return err
	})
}

func (r *FalVideoBillingRepository) withLockedTask(ctx context.Context, id string, apply func(*sql.Tx, falVideoBillingTask) error) error {
	if r == nil || r.usageBilling.db == nil {
		return errors.New("fal video billing repository db is nil")
	}
	// The reservation scan must see changes committed while waiting on the key lock.
	tx, err := r.usageBilling.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Use the immutable pricing snapshot and owner on the locked task, not the caller's copy.
	var task falVideoBillingTask
	err = tx.QueryRowContext(ctx, `SELECT state, billing_state, user_id, api_key_id, group_id, duration, unit_price_usd
		FROM fal_video_tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&task.state, &task.billingState, &task.userID, &task.apiKeyID, &task.groupID, &task.duration, &task.unitPriceUSD)
	if err != nil {
		return fmt.Errorf("lock fal video task for billing: %w", err)
	}
	if err := apply(tx, task); err != nil {
		return err
	}
	return tx.Commit()
}

func checkFalVideoPlatformQuota(ctx context.Context, tx *sql.Tx, userID, groupID int64) error {
	// Fal cannot reserve user-platform allowances yet. Check Grok as well as the effective group platform.
	var configured bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_platform_quotas q
		WHERE q.user_id = $1 AND q.deleted_at IS NULL
			AND q.platform IN ($4, CASE WHEN g.platform = $3 THEN $4 ELSE g.platform END)
			AND (q.daily_limit_usd IS NOT NULL OR q.weekly_limit_usd IS NOT NULL OR q.monthly_limit_usd IS NOT NULL)
	) FROM groups g WHERE g.id = $2 AND g.deleted_at IS NULL`, userID, groupID, service.PlatformComposite, service.PlatformGrok).Scan(&configured)
	if err != nil {
		return fmt.Errorf("check fal video platform quota: %w", err)
	}
	if configured {
		return service.ErrFalVideoKeyLimit
	}
	return nil
}

func checkFalVideoBillingAPIKey(ctx context.Context, tx *sql.Tx, taskID string, apiKeyID int64, amount float64) error {
	// Match the settlement helper's reset rules, including how it treats a NULL window start.
	var quota, limit5h, limit1d, limit7d, quotaUsed, used5h, used1d, used7d decimal.Decimal
	err := tx.QueryRowContext(ctx, `SELECT quota, rate_limit_5h, rate_limit_1d, rate_limit_7d, quota_used,
		CASE WHEN window_5h_start + INTERVAL '5 hours' <= NOW() THEN 0 ELSE usage_5h END,
		CASE WHEN window_1d_start + INTERVAL '24 hours' <= NOW() THEN 0 ELSE usage_1d END,
		CASE WHEN window_7d_start + INTERVAL '7 days' <= NOW() THEN 0 ELSE usage_7d END
		FROM api_keys WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, apiKeyID).
		Scan(&quota, &limit5h, &limit1d, &limit7d, &quotaUsed, &used5h, &used1d, &used7d)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrAPIKeyNotFound
	}
	if err != nil {
		return err
	}
	limits := [][2]decimal.Decimal{{quota, quotaUsed}, {limit5h, used5h}, {limit1d, used1d}, {limit7d, used7d}}
	var remaining decimal.Decimal
	limited := false
	for _, pair := range limits {
		limit, used := pair[0], pair[1]
		if limit.IsPositive() {
			left := limit.Sub(used)
			if !limited || left.LessThan(remaining) {
				remaining = left
			}
			limited = true
		}
	}
	if !limited {
		return nil
	}
	if remaining = remaining.Sub(usageBillingAmount(amount)); remaining.IsNegative() {
		return service.ErrFalVideoKeyLimit
	}

	// Do not lock other task rows: every reservation and settlement locks its own task first.
	// Sum in Go so each task uses exactly the same quantization as capture and release.
	rows, err := tx.QueryContext(ctx, `SELECT duration, unit_price_usd FROM fal_video_tasks
		WHERE api_key_id = $1 AND billing_state = 'reserved' AND id <> $2`, apiKeyID, taskID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var task falVideoBillingTask
		if err := rows.Scan(&task.duration, &task.unitPriceUSD); err != nil {
			return err
		}
		held, err := task.amount()
		if err != nil {
			return err
		}
		remaining = remaining.Sub(usageBillingAmount(held))
		if remaining.IsNegative() {
			return service.ErrFalVideoKeyLimit
		}
	}
	return rows.Err()
}

func incrementFalVideoBillingAPIKey(ctx context.Context, tx *sql.Tx, apiKeyID int64, amount float64) error {
	var hasQuota, hasRateLimits bool
	err := tx.QueryRowContext(ctx, `SELECT quota > 0, rate_limit_5h > 0 OR rate_limit_1d > 0 OR rate_limit_7d > 0
		FROM api_keys WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, apiKeyID).Scan(&hasQuota, &hasRateLimits)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if hasQuota {
		if _, err := incrementUsageBillingAPIKeyQuota(ctx, tx, apiKeyID, amount); err != nil {
			return err
		}
	}
	if hasRateLimits {
		return incrementUsageBillingAPIKeyRateLimit(ctx, tx, apiKeyID, amount)
	}
	return nil
}
