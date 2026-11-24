package falvideotask

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Task struct {
	ID, Model, Endpoint, Resolution, State, ResultURL, ErrorCode, IdempotencyKey, Fingerprint string
	UserID, APIKeyID, GroupID, SubscriptionID, AccountID                                      int64
	Duration                                                                                  int
	ActualDuration, UnitPriceUSD                                                              float64
	Width, Height                                                                             int
	Reference                                                                                 []byte
	CreatedAt, UpdatedAt, NextPollAt                                                          time.Time
	LeaseToken                                                                                string
}

var (
	ErrInvalidTransition = errors.New("invalid fal video task transition")
	ErrLeaseLost         = errors.New("fal video task lease lost")
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const taskColumns = `id, model, endpoint, resolution, state, result_url, error_code, idempotency_key, fingerprint,
	user_id, api_key_id, group_id, subscription_id, account_id, duration, actual_duration, unit_price_usd,
	width, height, reference, created_at, updated_at, next_poll_at, COALESCE(lease_token::text, '')`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var next sql.NullTime
	err := row.Scan(&t.ID, &t.Model, &t.Endpoint, &t.Resolution, &t.State, &t.ResultURL, &t.ErrorCode,
		&t.IdempotencyKey, &t.Fingerprint, &t.UserID, &t.APIKeyID, &t.GroupID, &t.SubscriptionID,
		&t.AccountID, &t.Duration, &t.ActualDuration, &t.UnitPriceUSD, &t.Width, &t.Height, &t.Reference,
		&t.CreatedAt, &t.UpdatedAt, &next, &t.LeaseToken)
	if err != nil {
		return Task{}, err
	}
	t.NextPollAt = next.Time
	return t, nil
}

// Create returns the stored task and whether this call inserted it. An empty key disables idempotency.
func (s *Store) Create(ctx context.Context, task Task) (Task, bool, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `
		INSERT INTO fal_video_tasks (id, model, endpoint, resolution, idempotency_key, fingerprint,
			user_id, api_key_id, group_id, subscription_id, account_id, duration, unit_price_usd)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (user_id, api_key_id, group_id, idempotency_key) WHERE idempotency_key <> '' DO NOTHING
		RETURNING `+taskColumns, task.ID, task.Model, task.Endpoint, task.Resolution,
		task.IdempotencyKey, task.Fingerprint, task.UserID, task.APIKeyID, task.GroupID,
		task.SubscriptionID, task.AccountID, task.Duration, task.UnitPriceUSD))
	if !errors.Is(err, sql.ErrNoRows) {
		return t, err == nil, err
	}
	// A separate statement sees a concurrent insert that committed after the INSERT snapshot.
	t, err = scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM fal_video_tasks
		WHERE user_id = $1 AND api_key_id = $2 AND group_id = $3 AND idempotency_key = $4`,
		task.UserID, task.APIKeyID, task.GroupID, task.IdempotencyKey))
	return t, false, err
}

func (s *Store) Get(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM fal_video_tasks WHERE id = $1`, id))
}

func (s *Store) GetByIdempotencyKey(ctx context.Context, userID, keyID, groupID int64, key string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM fal_video_tasks
		WHERE user_id = $1 AND api_key_id = $2 AND group_id = $3 AND idempotency_key = $4 AND idempotency_key <> ''`, userID, keyID, groupID, key))
}

func (s *Store) Accept(ctx context.Context, id string, ref []byte) error {
	return s.update(ctx, ErrInvalidTransition, `UPDATE fal_video_tasks
		SET state = 'queued', reference = $2::json, next_poll_at = statement_timestamp(), updated_at = statement_timestamp()
		WHERE id = $1 AND state = 'submitting'`, id, string(ref))
}

func (s *Store) Reject(ctx context.Context, id, state, code string) error {
	if state != "failed" && state != "unknown" && state != "pending_failure" {
		return ErrInvalidTransition
	}
	return s.update(ctx, ErrInvalidTransition, `UPDATE fal_video_tasks
		SET state = $2::text, error_code = $3, updated_at = statement_timestamp(),
			next_poll_at = CASE WHEN $2 = 'pending_failure' THEN statement_timestamp() END
		WHERE id = $1 AND state = 'submitting'`, id, state, code)
}

func (s *Store) Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error) {
	if limit <= 0 {
		return nil, nil
	}
	if lease <= 0 {
		return nil, errors.New("fal video task lease must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `WITH due AS (
		SELECT id FROM fal_video_tasks
		WHERE state IN ('queued', 'running', 'pending_success', 'pending_failure')
		  AND next_poll_at <= statement_timestamp()
		  AND (lease_until IS NULL OR lease_until <= statement_timestamp())
		ORDER BY next_poll_at, id LIMIT $1 FOR UPDATE SKIP LOCKED
	)
	UPDATE fal_video_tasks
	SET lease_token = gen_random_uuid(), lease_until = clock_timestamp() + $2::bigint * interval '1 microsecond',
		updated_at = statement_timestamp()
	WHERE id IN (SELECT id FROM due)
	RETURNING `+taskColumns, limit, 1+(lease-1).Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *Store) Reschedule(ctx context.Context, id, leaseToken, state string, next time.Time) error {
	return s.update(ctx, ErrLeaseLost, `UPDATE fal_video_tasks
		SET state = $3::text, next_poll_at = $4, lease_token = NULL, lease_until = NULL, updated_at = statement_timestamp()
		WHERE id = $1 AND lease_token::text = $2 AND lease_until > clock_timestamp()
		  AND ((state IN ('queued', 'running') AND $3 IN ('queued', 'running'))
		       OR (state IN ('pending_success', 'pending_failure') AND $3 = state))`, id, leaseToken, state, next)
}

// Finish records a poll outcome or finalizes a previously recorded settlement outcome.
// The latter preserves the poll result, so settlement retries do not need to supply it again.
func (s *Store) Finish(ctx context.Context, id, leaseToken, state, resultURL string, duration float64, width, height int, errorCode string) error {
	return s.update(ctx, ErrLeaseLost, `UPDATE fal_video_tasks
		SET state = $3::text,
			result_url = CASE WHEN state IN ('queued', 'running') THEN $4 ELSE result_url END,
			actual_duration = CASE WHEN state IN ('queued', 'running') THEN $5 ELSE actual_duration END,
			width = CASE WHEN state IN ('queued', 'running') THEN $6 ELSE width END,
			height = CASE WHEN state IN ('queued', 'running') THEN $7 ELSE height END,
			error_code = CASE WHEN state IN ('queued', 'running') THEN $8 ELSE error_code END,
			next_poll_at = CASE WHEN $3 IN ('pending_success', 'pending_failure') THEN statement_timestamp() END,
			lease_token = NULL, lease_until = NULL, updated_at = statement_timestamp()
		WHERE id = $1 AND lease_token::text = $2 AND lease_until > clock_timestamp()
		  AND ((state IN ('queued', 'running') AND $3 IN ('pending_success', 'pending_failure', 'succeeded', 'failed', 'unknown'))
		       OR (state = 'pending_success' AND $3 = 'succeeded')
		       OR (state = 'pending_failure' AND $3 IN ('failed', 'unknown')))`,
		id, leaseToken, state, resultURL, duration, width, height, errorCode)
}

func (s *Store) MarkStaleSubmissions(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fal_video_tasks
		SET state = 'pending_failure', error_code = 'submission_unknown',
			next_poll_at = statement_timestamp(), updated_at = statement_timestamp()
		WHERE state = 'submitting' AND updated_at < $1`, olderThan)
	return err
}

func (s *Store) update(ctx context.Context, noMatch error, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return noMatch
	}
	return err
}
