//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestVideoTaskBillingCapture(t *testing.T) {
	ctx := context.Background()
	store, task := createVideoTaskBillingTask(t, 5, 1, 1)
	repo := NewVideoTaskBillingRepository(integrationDB)

	// Settlement must use the persisted snapshot, not the caller's copy or actual duration.
	input := videotask.Task{ID: task.ID, UserID: -1, Duration: 99, QuotedAmountUSD: 10}
	repeatVideoTaskBilling(t, func() error { return repo.Reserve(ctx, input) })
	assertVideoTaskBilling(t, task, "reserved", "4.62962963", "0.37037037", "0.00000000", "0.00000000")
	require.ErrorIs(t, repo.Settle(ctx, input, false), videotask.ErrInvalidTransition)

	require.NoError(t, store.Accept(ctx, task.ID, []byte(`{}`)))
	setVideoTaskBillingPending(t, task.ID, "pending_success")
	repeatVideoTaskBilling(t, func() error { return repo.Settle(ctx, input, true) })
	require.ErrorIs(t, repo.Settle(ctx, input, false), videotask.ErrInvalidTransition)
	assertVideoTaskBilling(t, task, "captured", "4.62962963", "0.00000000", "0.37037037", "0.37037037")
}

func TestVideoTaskBillingUsesProfileQuoteForHoldsAndCapture(t *testing.T) {
	ctx := context.Background()
	store, task := createVideoTaskBillingTask(t, 5, 1, 1)
	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET profile_version = 'veo31-v1', result_path = 'video', quoted_amount_usd = 0.625 WHERE id = $1`, task.ID)
	require.NoError(t, err)
	stored, err := store.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, 0.625, stored.QuotedAmountUSD)
	require.Equal(t, "video", stored.ResultPath)
	repo := NewVideoTaskBillingRepository(integrationDB)
	repeatVideoTaskBilling(t, func() error { return repo.Reserve(ctx, task) })
	assertVideoTaskBilling(t, task, "reserved", "4.37500000", "0.62500000", "0.00000000", "0.00000000")
	setVideoTaskBillingPending(t, task.ID, "pending_success")
	repeatVideoTaskBilling(t, func() error { return repo.Settle(ctx, task, true) })
	assertVideoTaskBilling(t, task, "captured", "4.37500000", "0.00000000", "0.62500000", "0.62500000")
}

func TestVideoTaskBillingReleasesStaleSubmission(t *testing.T) {
	ctx := context.Background()
	store, task := createVideoTaskBillingTask(t, 5, 1, 1)
	repo := NewVideoTaskBillingRepository(integrationDB)
	require.NoError(t, repo.Reserve(ctx, task))
	require.NoError(t, store.MarkStaleSubmissions(ctx, service.PlatformFal, time.Now().Add(-time.Hour)))
	require.ErrorIs(t, repo.Settle(ctx, task, false), videotask.ErrInvalidTransition)
	assertVideoTaskBilling(t, task, "reserved", "4.62962963", "0.37037037", "0.00000000", "0.00000000")

	_, err := integrationDB.ExecContext(ctx, `UPDATE video_tasks SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, task.ID)
	require.NoError(t, err)
	require.NoError(t, store.MarkStaleSubmissions(ctx, service.PlatformFal, time.Now().Add(-time.Hour)))
	require.ErrorIs(t, repo.Reserve(ctx, task), videotask.ErrInvalidTransition)
	repeatVideoTaskBilling(t, func() error { return repo.Settle(ctx, task, false) })
	assertVideoTaskBilling(t, task, "released", "5.00000000", "0.00000000", "0.00000000", "0.00000000")
}

func TestVideoTaskBillingInsufficientBalanceCannotCaptureOrReleaseFunds(t *testing.T) {
	ctx := context.Background()
	_, task := createVideoTaskBillingTask(t, 0.25, 1, 1)
	repo := NewVideoTaskBillingRepository(integrationDB)
	_, err := integrationDB.ExecContext(ctx, `UPDATE users SET frozen_balance = 1 WHERE id = $1`, task.UserID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.Reserve(ctx, task), service.ErrBatchImageInsufficientBalance)
	assertVideoTaskBilling(t, task, "unreserved", "0.25000000", "1.00000000", "0.00000000", "0.00000000")

	setVideoTaskBillingPending(t, task.ID, "pending_success")
	require.ErrorIs(t, repo.Settle(ctx, task, true), videotask.ErrInvalidTransition)
	setVideoTaskBillingPending(t, task.ID, "pending_failure")
	require.NoError(t, repo.Settle(ctx, task, false))
	assertVideoTaskBilling(t, task, "released", "0.25000000", "1.00000000", "0.00000000", "0.00000000")
}

func TestVideoTaskBillingCaptureWithUnlimitedAPIKey(t *testing.T) {
	ctx := context.Background()
	_, task := createVideoTaskBillingTask(t, 5, 0, 0)
	repo := NewVideoTaskBillingRepository(integrationDB)
	require.NoError(t, repo.Reserve(ctx, task))
	setVideoTaskBillingPending(t, task.ID, "pending_success")
	require.NoError(t, repo.Settle(ctx, task, true))
	assertVideoTaskBilling(t, task, "captured", "4.62962963", "0.00000000", "0.00000000", "0.00000000")
}

func TestVideoTaskBillingCaptureRollsBackWhenKeyChargeFails(t *testing.T) {
	ctx := context.Background()
	_, task := createVideoTaskBillingTask(t, 5, 1, 1)
	repo := NewVideoTaskBillingRepository(integrationDB)
	require.NoError(t, repo.Reserve(ctx, task))
	setVideoTaskBillingPending(t, task.ID, "pending_success")

	_, err := integrationDB.ExecContext(ctx, `UPDATE api_keys SET usage_5h = 999999999999.99999999 WHERE id = $1`, task.APIKeyID)
	require.NoError(t, err)
	require.Error(t, repo.Settle(ctx, task, true))
	assertVideoTaskBilling(t, task, "reserved", "4.62962963", "0.37037037", "0.00000000", "999999999999.99999999")

	_, err = integrationDB.ExecContext(ctx, `UPDATE api_keys SET usage_5h = 0 WHERE id = $1`, task.APIKeyID)
	require.NoError(t, err)
	require.NoError(t, repo.Settle(ctx, task, true))
	assertVideoTaskBilling(t, task, "captured", "4.62962963", "0.00000000", "0.37037037", "0.37037037")
}

func TestVideoTaskBillingConcurrentReservesRespectKeyLimits(t *testing.T) {
	for _, limit := range []string{"quota", "rate_limit_5h", "rate_limit_1d", "rate_limit_7d"} {
		t.Run(limit, func(t *testing.T) {
			ctx := context.Background()
			store, first := createVideoTaskBillingTask(t, 5, 0, 0)
			_, err := integrationDB.ExecContext(ctx, `UPDATE api_keys SET `+limit+` = 0.74074074 WHERE id = $1`, first.APIKeyID)
			require.NoError(t, err)
			repo := NewVideoTaskBillingRepository(integrationDB)
			other := first
			other.ID = uuid.NewString()
			second, created, err := store.Create(ctx, other)
			require.NoError(t, err)
			require.True(t, created)
			other.ID = uuid.NewString()
			third, created, err := store.Create(ctx, other)
			require.NoError(t, err)
			require.True(t, created)
			start := make(chan struct{})
			errs := make(chan error, 3)
			for _, task := range []videotask.Task{first, second, third} {
				go func() {
					<-start
					errs <- repo.Reserve(ctx, task)
				}()
			}
			close(start)
			var accepted int
			for range 3 {
				if err := <-errs; err == nil {
					accepted++
				} else {
					require.ErrorIs(t, err, service.ErrVideoTaskKeyLimit)
				}
			}
			require.Equal(t, 2, accepted)
			var balance, frozen string
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance, frozen_balance FROM users WHERE id = $1`, first.UserID).Scan(&balance, &frozen))
			require.Equal(t, "4.25925926", balance)
			require.Equal(t, "0.74074074", frozen)
		})
	}
}

func TestVideoTaskBillingReserveChecksPersistedWindowUsage(t *testing.T) {
	ctx := context.Background()
	_, task := createVideoTaskBillingTask(t, 5, 0, 1)
	repo := NewVideoTaskBillingRepository(integrationDB)
	_, err := integrationDB.ExecContext(ctx, `UPDATE api_keys SET usage_5h = 0.75, window_5h_start = NOW() WHERE id = $1`, task.APIKeyID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.Reserve(ctx, task), service.ErrVideoTaskKeyLimit)
	assertVideoTaskBilling(t, task, "unreserved", "5.00000000", "0.00000000", "0.00000000", "0.75000000")
	_, err = integrationDB.ExecContext(ctx, `UPDATE api_keys SET window_5h_start = NOW() - INTERVAL '6 hours' WHERE id = $1`, task.APIKeyID)
	require.NoError(t, err)
	require.NoError(t, repo.Reserve(ctx, task))
	setVideoTaskBillingPending(t, task.ID, "pending_success")
	require.NoError(t, repo.Settle(ctx, task, true))
	assertVideoTaskBilling(t, task, "captured", "4.62962963", "0.00000000", "0.00000000", "0.37037037")
}

func TestVideoTaskBillingReleaseRestoresKeyCapacityButCaptureDoesNot(t *testing.T) {
	ctx := context.Background()
	store, first := createVideoTaskBillingTask(t, 5, 0.37037037, 0)
	repo := NewVideoTaskBillingRepository(integrationDB)
	other := first
	other.ID = uuid.NewString()
	second, created, err := store.Create(ctx, other)
	require.NoError(t, err)
	require.True(t, created)

	require.NoError(t, repo.Reserve(ctx, first))
	require.ErrorIs(t, repo.Reserve(ctx, second), service.ErrVideoTaskKeyLimit)
	setVideoTaskBillingPending(t, first.ID, "pending_failure")
	require.NoError(t, repo.Settle(ctx, first, false))
	require.NoError(t, repo.Reserve(ctx, second))
	setVideoTaskBillingPending(t, second.ID, "pending_success")
	require.NoError(t, repo.Settle(ctx, second, true))

	other.ID = uuid.NewString()
	third, created, err := store.Create(ctx, other)
	require.NoError(t, err)
	require.True(t, created)
	require.ErrorIs(t, repo.Reserve(ctx, third), service.ErrVideoTaskKeyLimit)
	assertVideoTaskBilling(t, third, "unreserved", "4.62962963", "0.00000000", "0.37037037", "0.00000000")
}

func TestVideoTaskBillingReserveFailsClosedForConfiguredPlatformQuotas(t *testing.T) {
	for _, tc := range []struct {
		name, groupPlatform, quotaPlatform string
		daily, weekly, monthly             any
		deleted, blocked                   bool
	}{
		{name: "group platform daily", groupPlatform: service.PlatformOpenAI, quotaPlatform: service.PlatformOpenAI, daily: 10, blocked: true},
		{name: "composite maps to Grok", groupPlatform: service.PlatformComposite, quotaPlatform: service.PlatformGrok, weekly: 0, blocked: true},
		{name: "Grok checked for other groups", groupPlatform: service.PlatformOpenAI, quotaPlatform: service.PlatformGrok, monthly: 10, blocked: true},
		{name: "all limits null", groupPlatform: service.PlatformComposite, quotaPlatform: service.PlatformGrok},
		{name: "soft deleted", groupPlatform: service.PlatformComposite, quotaPlatform: service.PlatformGrok, daily: 0, deleted: true},
		{name: "unrelated platform", groupPlatform: service.PlatformOpenAI, quotaPlatform: service.PlatformAnthropic, daily: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, task := createVideoTaskBillingTask(t, 5, 0, 0)
			repo := NewVideoTaskBillingRepository(integrationDB)
			_, err := integrationDB.ExecContext(ctx, `UPDATE groups SET platform = $2 WHERE id = $1`, task.GroupID, tc.groupPlatform)
			require.NoError(t, err)
			_, err = integrationDB.ExecContext(ctx, `INSERT INTO user_platform_quotas
				(user_id, platform, daily_limit_usd, weekly_limit_usd, monthly_limit_usd, deleted_at)
				VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN NOW() END)`, task.UserID, tc.quotaPlatform, tc.daily, tc.weekly, tc.monthly, tc.deleted)
			require.NoError(t, err)
			if tc.blocked {
				require.ErrorIs(t, repo.Reserve(ctx, task), service.ErrVideoTaskKeyLimit)
				assertVideoTaskBilling(t, task, "unreserved", "5.00000000", "0.00000000", "0.00000000", "0.00000000")
				return
			}
			require.NoError(t, repo.Reserve(ctx, task))
			assertVideoTaskBilling(t, task, "reserved", "4.62962963", "0.37037037", "0.00000000", "0.00000000")
		})
	}
}

func createVideoTaskBillingTask(t *testing.T, balance, quota, rateLimit float64) (*videotask.Store, videotask.Task) {
	t.Helper()
	ctx := context.Background()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{
		Email: uuid.NewString() + "@example.com", PasswordHash: "hash", Balance: balance,
	})
	group := mustCreateGroup(t, client, &service.Group{Name: "fal-video-" + uuid.NewString(), Platform: service.PlatformComposite})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, GroupID: &group.ID, Key: "sk-fal-video-" + uuid.NewString(), Quota: quota, RateLimit5h: rateLimit,
	})
	store := videotask.NewStore(integrationDB)
	task, created, err := store.Create(ctx, videotask.Task{
		ID: uuid.NewString(), UserID: user.ID, APIKeyID: apiKey.ID, GroupID: group.ID, AccountID: 1,
		Model: "test-video", Endpoint: "test/video", Resolution: "720p", Duration: 3, QuotedAmountUSD: 0.37037037, Provider: service.PlatformFal, ProfileVersion: "test-v1", ResultPath: "video",
	})
	require.NoError(t, err)
	require.True(t, created)
	return store, task
}

func setVideoTaskBillingPending(t *testing.T, id, state string) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE video_tasks
		SET state = $2, next_poll_at = NOW(), actual_duration = 1 WHERE id = $1`, id, state)
	require.NoError(t, err)
}

func repeatVideoTaskBilling(t *testing.T, action func() error) {
	t.Helper()
	errs := make([]error, 6)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = action()
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
}

func assertVideoTaskBilling(t *testing.T, task videotask.Task, wantState, wantBalance, wantFrozen, wantQuota, wantRate string) {
	t.Helper()
	var state, balance, frozen, quota, rate string
	err := integrationDB.QueryRowContext(context.Background(), `SELECT t.billing_state, u.balance, u.frozen_balance, k.quota_used, k.usage_5h
		FROM video_tasks t JOIN users u ON u.id = t.user_id JOIN api_keys k ON k.id = t.api_key_id
		WHERE t.id = $1`, task.ID).Scan(&state, &balance, &frozen, &quota, &rate)
	require.NoError(t, err)
	require.Equal(t, wantState, state)
	require.Equal(t, wantBalance, balance)
	require.Equal(t, wantFrozen, frozen)
	require.Equal(t, wantQuota, quota)
	require.Equal(t, wantRate, rate)
}
