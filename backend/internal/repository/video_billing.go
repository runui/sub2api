package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type VideoTaskBillingRepository struct {
	usageBilling usageBillingRepository
}

func NewVideoTaskBillingRepository(db *sql.DB) *VideoTaskBillingRepository {
	return &VideoTaskBillingRepository{usageBilling: usageBillingRepository{db: db}}
}

type videoTaskBillingTask struct {
	amount                        float64
	state, billingState, provider string
	userID, apiKeyID, groupID     int64
}

// Reserve freezes the full requested cost before upstream submission.
func (r *VideoTaskBillingRepository) Reserve(ctx context.Context, t videotask.Task) error {
	return r.withLockedTask(ctx, t.ID, func(tx *sql.Tx, task videoTaskBillingTask) error {
		if task.state != "submitting" {
			return videotask.ErrInvalidTransition
		}
		switch task.billingState {
		case "reserved":
			return nil
		case "unreserved":
		default:
			return videotask.ErrInvalidTransition
		}

		amount := task.amount
		_, err := reserveUsageBillingBatchImageBalance(ctx, tx, &service.BatchImageBalanceHoldCommand{
			UserID: task.userID, HoldAmount: amount,
		})
		if err != nil {
			return err
		}
		if err := checkVideoTaskPlatformQuota(ctx, tx, task.userID, task.groupID, task.provider); err != nil {
			return err
		}
		if err := checkVideoTaskBillingAPIKey(ctx, tx, t.ID, task.apiKeyID, amount); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE video_tasks
			SET billing_state = 'reserved', updated_at = clock_timestamp() WHERE id = $1`, t.ID)
		return err
	})
}

// Settle captures a successful task's reservation or releases it on failure.
func (r *VideoTaskBillingRepository) Settle(ctx context.Context, t videotask.Task, success bool) error {
	pending, terminal := "pending_failure", "released"
	if success {
		pending, terminal = "pending_success", "captured"
	}
	return r.withLockedTask(ctx, t.ID, func(tx *sql.Tx, task videoTaskBillingTask) error {
		if task.billingState == terminal {
			return nil
		}
		if task.state != pending {
			return videotask.ErrInvalidTransition
		}
		if task.billingState != "reserved" && !(task.billingState == "unreserved" && !success) {
			return videotask.ErrInvalidTransition
		}

		if task.billingState == "reserved" {
			amount := task.amount
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
				return errors.New("video task frozen balance is insufficient or user is missing")
			}
			if success {
				if err := incrementVideoTaskBillingAPIKey(ctx, tx, task.apiKeyID, amount); err != nil {
					return err
				}
			}
		}

		_, err := tx.ExecContext(ctx, `UPDATE video_tasks SET billing_state = $2 WHERE id = $1`, t.ID, terminal)
		return err
	})
}

func (r *VideoTaskBillingRepository) withLockedTask(ctx context.Context, id string, apply func(*sql.Tx, videoTaskBillingTask) error) error {
	if r == nil || r.usageBilling.db == nil {
		return errors.New("video task billing repository db is nil")
	}
	// The reservation scan must see changes committed while waiting on the key lock.
	tx, err := r.usageBilling.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Use the immutable pricing snapshot and owner on the locked task, not the caller's copy.
	var task videoTaskBillingTask
	err = tx.QueryRowContext(ctx, `SELECT state, billing_state, user_id, api_key_id, group_id, provider, quoted_amount_usd
		FROM video_tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&task.state, &task.billingState, &task.userID, &task.apiKeyID, &task.groupID, &task.provider, &task.amount)
	if err != nil {
		return fmt.Errorf("lock video task for billing: %w", err)
	}
	if err := apply(tx, task); err != nil {
		return err
	}
	return tx.Commit()
}

func checkVideoTaskPlatformQuota(ctx context.Context, tx *sql.Tx, userID, groupID int64, provider string) error {
	// Async tasks cannot reserve platform allowances yet. Fail closed for each
	// applicable allowance namespace, including any shared by the executor.
	var configured bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_platform_quotas q
		WHERE q.user_id = $1 AND q.deleted_at IS NULL
			AND q.platform IN ($3, $4, g.platform)
			AND (q.daily_limit_usd IS NOT NULL OR q.weekly_limit_usd IS NOT NULL OR q.monthly_limit_usd IS NOT NULL)
	) FROM groups g WHERE g.id = $2 AND g.deleted_at IS NULL`, userID, groupID, provider, service.VideoTaskQuotaPlatform(provider)).Scan(&configured)
	if err != nil {
		return fmt.Errorf("check video task platform quota: %w", err)
	}
	if configured {
		return service.ErrVideoTaskKeyLimit
	}
	return nil
}

func checkVideoTaskBillingAPIKey(ctx context.Context, tx *sql.Tx, taskID string, apiKeyID int64, amount float64) error {
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
		return service.ErrVideoTaskKeyLimit
	}

	// The key lock serializes reservations; do not also lock other task rows.
	var held decimal.Decimal
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(quoted_amount_usd), 0) FROM video_tasks
		WHERE api_key_id = $1 AND billing_state = 'reserved' AND id <> $2`, apiKeyID, taskID).Scan(&held); err != nil {
		return err
	}
	if remaining.LessThan(held) {
		return service.ErrVideoTaskKeyLimit
	}
	return nil
}

func incrementVideoTaskBillingAPIKey(ctx context.Context, tx *sql.Tx, apiKeyID int64, amount float64) error {
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
