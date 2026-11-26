package videotask

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func postgresStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, ctx := postgresBeforeVideoTaskMigration(t)
	must(t, applyTaskMigration(ctx, db, "257_video_tasks.sql"))
	return NewStore(db), ctx
}

func postgresBeforeVideoTaskMigration(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("VIDEO_TASK_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set VIDEO_TASK_TEST_POSTGRES_URL to run against PostgreSQL")
	}
	u, err := url.Parse(dsn)
	must(t, err)
	admin, err := sql.Open("postgres", dsn)
	must(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	schema := fmt.Sprintf("video_task_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema)
	must(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		if err != nil {
			t.Error(err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `CREATE TABLE composite_model_routes (endpoint text, target_platform text); CREATE TABLE users (id bigint PRIMARY KEY, balance numeric(24,8), frozen_balance numeric(24,8), updated_at timestamptz)`)
	must(t, err)
	must(t, applyTaskMigration(ctx, db, "255_create_fal_video_tasks.sql"))
	return db, ctx
}

func applyTaskMigration(ctx context.Context, db *sql.DB, file string) error {
	migration, err := os.ReadFile("../../../migrations/" + file)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
		return err
	}
	return tx.Commit()
}

func TestStoreMigrationDiscardsOldTasksAndRefundsOnlyOutstandingHolds(t *testing.T) {
	db, ctx := postgresBeforeVideoTaskMigration(t)
	_, err := db.ExecContext(ctx, `INSERT INTO users VALUES (10, 1, 2, NOW());
		INSERT INTO fal_video_tasks
		(id, user_id, api_key_id, group_id, account_id, model, endpoint, resolution, duration, unit_price_usd, billing_state, idempotency_key, fingerprint)
		VALUES ('old', 10, 20, 30, 40, 'video', 'test/video', '720p', 3, 0.123456789, 'reserved', 'old-key', 'old-fingerprint'),
	       ('captured', 10, 20, 30, 40, 'video', 'test/video', '720p', 3, 0.5, 'captured', '', 'old-fingerprint')`)
	must(t, err)
	_, err = db.ExecContext(ctx, `UPDATE users SET frozen_balance = 0 WHERE id = 10`)
	must(t, err)
	if err := applyTaskMigration(ctx, db, "257_video_tasks.sql"); err == nil {
		t.Fatal("reset must not delete tasks if their holds cannot be refunded")
	}
	var count int
	must(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fal_video_tasks`).Scan(&count))
	if count != 2 {
		t.Fatalf("failed reset changed legacy task rows: %d", count)
	}
	_, err = db.ExecContext(ctx, `UPDATE users SET frozen_balance = 2 WHERE id = 10`)
	must(t, err)
	must(t, applyTaskMigration(ctx, db, "257_video_tasks.sql"))
	must(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks`).Scan(&count))
	var balance, frozen string
	must(t, db.QueryRowContext(ctx, `SELECT balance::text, frozen_balance::text FROM users WHERE id = 10`).Scan(&balance, &frozen))
	if count != 0 || balance != "1.37037037" || frozen != "1.62962963" {
		t.Fatalf("unexpected reset: rows=%d, balance=%s, frozen=%s", count, balance, frozen)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sampleTask(id, key string) Task {
	return Task{ID: id, UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: 40, SubscriptionID: 50,
		Model: "video", Endpoint: "test/video", Resolution: "720p", Duration: 5, QuotedAmountUSD: 1.25, Provider: "fal", ProfileVersion: "test-v1", ResultPath: "video",
		IdempotencyKey: key, Fingerprint: "fingerprint-" + id}
}

func createTask(t *testing.T, s *Store, ctx context.Context, task Task) Task {
	t.Helper()
	got, inserted, err := s.Create(ctx, task)
	must(t, err)
	if !inserted || got.ID != task.ID || got.State != "submitting" || got.LeaseToken != "" || !got.NextPollAt.IsZero() {
		t.Fatalf("unexpected Create result: %+v (inserted=%v)", got, inserted)
	}
	return got
}

func claimOne(t *testing.T, s *Store, ctx context.Context) Task {
	t.Helper()
	tasks, err := s.Claim(ctx, "fal", 1, time.Minute)
	must(t, err)
	if len(tasks) != 1 || tasks[0].LeaseToken == "" {
		t.Fatalf("expected one leased task, got %+v", tasks)
	}
	return tasks[0]
}

func claimNone(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	tasks, err := s.Claim(ctx, "fal", 10, time.Minute)
	must(t, err)
	if len(tasks) != 0 {
		t.Fatalf("expected no eligible tasks, got %+v", tasks)
	}
}

func TestStoreCreateIdempotency(t *testing.T) {
	s, ctx := postgresStore(t)
	const workers = 8
	type result struct {
		task     Task
		inserted bool
		err      error
	}
	results := make(chan result, workers)
	start := make(chan struct{})
	for i := range workers {
		go func() {
			<-start
			task, inserted, err := s.Create(ctx, sampleTask(fmt.Sprint(i), "same-key"))
			results <- result{task, inserted, err}
		}()
	}
	close(start)
	var winner Task
	insertions := 0
	for i := range workers {
		r := <-results
		must(t, r.err)
		if i == 0 {
			winner = r.task
		}
		if r.task.ID != winner.ID || r.task.Fingerprint != winner.Fingerprint {
			t.Fatalf("idempotent call returned %+v, expected %+v", r.task, winner)
		}
		if r.inserted {
			insertions++
		}
	}
	if insertions != 1 || winner.AccountID != 40 || winner.SubscriptionID != 50 || winner.QuotedAmountUSD != 1.25 {
		t.Fatalf("unexpected insertion count (%d) or price/account snapshot: %+v", insertions, winner)
	}
	var billingState string
	must(t, s.db.QueryRowContext(ctx, `SELECT billing_state FROM video_tasks WHERE id = $1`, winner.ID).Scan(&billingState))
	if billingState != "unreserved" {
		t.Fatalf("default billing state = %q", billingState)
	}
	for id, change := range map[string]func(*Task){
		"different-user":  func(t *Task) { t.UserID++ },
		"different-key":   func(t *Task) { t.APIKeyID++ },
		"different-group": func(t *Task) { t.GroupID++ },
	} {
		task := sampleTask(id, "same-key")
		change(&task)
		createTask(t, s, ctx, task)
	}
	createTask(t, s, ctx, sampleTask("unkeyed-a", ""))
	createTask(t, s, ctx, sampleTask("unkeyed-b", ""))
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing Get error = %v", err)
	}
}

func TestStoreWorkersOnlyHandleTheirProvider(t *testing.T) {
	s, ctx := postgresStore(t)
	first := sampleTask("first", "shared-key")
	createTask(t, s, ctx, first)
	other := sampleTask("other", "shared-key")
	other.Provider = "other"
	got, inserted, err := s.Create(ctx, other)
	must(t, err)
	if inserted || got.ID != first.ID || got.Provider != first.Provider {
		t.Fatalf("cross-provider retry did not return the original task: %+v", got)
	}
	other.IdempotencyKey = ""
	createTask(t, s, ctx, other)
	must(t, s.Accept(ctx, other.ID, []byte(`{}`)))
	claimNone(t, s, ctx)
	jobs, err := s.Claim(ctx, other.Provider, 1, time.Minute)
	must(t, err)
	if len(jobs) != 1 || jobs[0].ID != other.ID {
		t.Fatalf("expected only the other provider task, got %+v", jobs)
	}
	stale := sampleTask("other-stale", "")
	stale.Provider = other.Provider
	createTask(t, s, ctx, stale)
	_, err = s.db.ExecContext(ctx, `UPDATE video_tasks SET updated_at = CURRENT_TIMESTAMP - interval '2 hours' WHERE id IN ('first', 'other-stale')`)
	must(t, err)
	must(t, s.MarkStaleSubmissions(ctx, first.Provider, time.Now().Add(-time.Hour)))
	if job := claimOne(t, s, ctx); job.ID != first.ID || job.State != "pending_failure" {
		t.Fatalf("expected stale task from the worker's provider, got %+v", job)
	}
	got, err = s.Get(ctx, stale.ID)
	must(t, err)
	if got.State != "submitting" {
		t.Fatalf("other provider's submission changed state: %+v", got)
	}
}

func TestStoreLeaseAndSettlement(t *testing.T) {
	s, ctx := postgresStore(t)
	createTask(t, s, ctx, sampleTask("video", ""))
	claimNone(t, s, ctx)
	ref := []byte(`{ "poll_id": "opaque" }`)
	must(t, s.Accept(ctx, "video", ref))
	if err := s.Accept(ctx, "video", ref); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate Accept error = %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SELECT id FROM video_tasks WHERE id = 'video' FOR UPDATE`)
	must(t, err)
	claimNone(t, s, ctx) // A different transaction must skip this row without waiting.
	must(t, tx.Rollback())
	first := claimOne(t, s, ctx)
	if first.State != "queued" || string(first.Reference) != string(ref) {
		t.Fatalf("unexpected queued task: %+v", first)
	}
	claimNone(t, s, ctx)
	_, err = s.db.ExecContext(ctx, `UPDATE video_tasks SET lease_until = CURRENT_TIMESTAMP - interval '1 second' WHERE id = 'video'`)
	must(t, err)
	if err := s.Reschedule(ctx, first.ID, first.LeaseToken, "running", time.Now()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease Reschedule error = %v", err)
	}
	second := claimOne(t, s, ctx)
	if second.LeaseToken == first.LeaseToken {
		t.Fatal("reclaimed task reused the previous token")
	}
	if err := s.Finish(ctx, first.ID, first.LeaseToken, "failed", "", 0, 0, 0, "stale"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale lease Finish error = %v", err)
	}
	must(t, s.Reschedule(ctx, second.ID, second.LeaseToken, "running", time.Now().Add(-time.Minute)))
	running := claimOne(t, s, ctx)
	const resultURL = "https://example.test/video.mp4"
	must(t, s.Finish(ctx, running.ID, running.LeaseToken, "pending_success", resultURL, 4.75, 1280, 720, ""))
	pending := claimOne(t, s, ctx)
	if pending.State != "pending_success" || pending.ResultURL != resultURL || pending.ActualDuration != 4.75 || pending.Width != 1280 || pending.Height != 720 {
		t.Fatalf("pending outcome not persisted: %+v", pending)
	}
	must(t, s.Reschedule(ctx, pending.ID, pending.LeaseToken, pending.State, time.Now().Add(time.Hour)))
	claimNone(t, s, ctx)
	_, err = s.db.ExecContext(ctx, `UPDATE video_tasks SET next_poll_at = CURRENT_TIMESTAMP WHERE id = 'video'`)
	must(t, err)
	retry := claimOne(t, s, ctx)
	must(t, s.Finish(ctx, retry.ID, retry.LeaseToken, "succeeded", "", 0, 0, 0, ""))
	final, err := s.Get(ctx, retry.ID)
	must(t, err)
	if final.State != "succeeded" || final.ResultURL != resultURL || final.ActualDuration != 4.75 || final.Width != 1280 || final.Height != 720 || !final.NextPollAt.IsZero() || final.LeaseToken != "" {
		t.Fatalf("unexpected finalized result: %+v", final)
	}
	claimNone(t, s, ctx)
}

func TestStoreRecoversStaleSubmissions(t *testing.T) {
	s, ctx := postgresStore(t)
	fresh := createTask(t, s, ctx, sampleTask("fresh", ""))
	createTask(t, s, ctx, sampleTask("stale", ""))
	createTask(t, s, ctx, sampleTask("rejected", ""))
	_, err := s.db.ExecContext(ctx, `UPDATE video_tasks SET updated_at = CURRENT_TIMESTAMP - interval '2 hours' WHERE id = 'stale'`)
	must(t, err)
	must(t, s.MarkStaleSubmissions(ctx, "fal", fresh.CreatedAt.Add(-time.Hour)))
	if err := s.Accept(ctx, "stale", []byte(`{}`)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("late Accept error = %v", err)
	}
	stale := claimOne(t, s, ctx)
	if stale.ID != "stale" || stale.State != "pending_failure" || stale.ErrorCode != "submission_unknown" {
		t.Fatalf("unexpected recovery: %+v", stale)
	}
	must(t, s.Finish(ctx, stale.ID, stale.LeaseToken, "unknown", "", 0, 0, 0, ""))
	done, err := s.Get(ctx, stale.ID)
	must(t, err)
	if done.State != "unknown" || done.ErrorCode != stale.ErrorCode {
		t.Fatalf("unexpected finalized recovery: %+v", done)
	}
	must(t, s.Reject(ctx, "rejected", "pending_failure", "upstream_rejected"))
	rejected := claimOne(t, s, ctx)
	if rejected.ID != "rejected" || rejected.State != "pending_failure" || rejected.ErrorCode != "upstream_rejected" {
		t.Fatalf("unexpected rejection: %+v", rejected)
	}
	must(t, s.Finish(ctx, rejected.ID, rejected.LeaseToken, "failed", "", 0, 0, 0, ""))
	got, err := s.Get(ctx, fresh.ID)
	must(t, err)
	if got.State != "submitting" {
		t.Fatalf("fresh submission changed state: %+v", got)
	}
	claimNone(t, s, ctx)
}
