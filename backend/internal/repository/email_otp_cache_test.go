package repository

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestEmailOTPCacheLifecycle(t *testing.T) {
	legacy, redisClock, rdb := newMiniredisEmailCache(t)
	cache := NewEmailOTPCache(rdb)
	ctx := context.Background()
	const email, alias = "user@gmail.com", "u.ser+tag@googlemail.com"
	challenge := service.EmailOTPChallenge{
		ID: "first", Generation: "generation", CodeMAC: "first-mac",
		Subject: service.EmailOTPSubject{UserID: math.MaxInt64, AccountVersion: "snapshot"},
	}
	const ttl, cooldown = service.EmailOTPCodeTTL, service.EmailOTPResendCooldown
	issue := func(email string, challenge service.EmailOTPChallenge) time.Duration {
		t.Helper()
		wait, err := cache.Issue(ctx, email, challenge, ttl, cooldown)
		require.NoError(t, err)
		return wait
	}
	consume := func(id, generation, codeMAC string) *service.EmailOTPSubject {
		t.Helper()
		subject, err := cache.Consume(ctx, email, id, generation, codeMAC, service.EmailOTPMaxWrongAttempts)
		require.NoError(t, err)
		return subject
	}

	require.NoError(t, legacy.SetVerificationCode(ctx, email, &service.VerificationCodeData{Code: "123456"}, ttl*4))
	require.Zero(t, issue(email, challenge))
	require.Equal(t, cooldown, issue(alias, challenge))
	require.Nil(t, consume(challenge.ID, challenge.Generation, challenge.CodeMAC), "unsent code must not work")
	require.NoError(t, cache.MarkDelivered(ctx, email, challenge.ID, challenge.Generation))
	require.Nil(t, consume(challenge.ID, "different-generation", challenge.CodeMAC))

	redisClock.FastForward(cooldown)
	replacement := challenge
	replacement.ID, replacement.CodeMAC = "second", "second-mac"
	require.Zero(t, issue(email, replacement))
	require.NoError(t, cache.MarkDelivered(ctx, email, challenge.ID, challenge.Generation))
	require.NoError(t, cache.Discard(ctx, email, challenge.ID, challenge.Generation))
	require.Nil(t, consume(challenge.ID, challenge.Generation, challenge.CodeMAC))
	require.Nil(t, consume(replacement.ID, replacement.Generation, replacement.CodeMAC), "old worker must not activate the replacement")
	require.NoError(t, cache.MarkDelivered(ctx, email, replacement.ID, replacement.Generation))
	for range service.EmailOTPMaxWrongAttempts {
		require.Nil(t, consume(replacement.ID, replacement.Generation, "wrong"))
	}
	require.Nil(t, consume(replacement.ID, replacement.Generation, replacement.CodeMAC))
	require.Equal(t, cooldown, issue(email, replacement), "consumption must not reset the send limit")

	redisClock.FastForward(cooldown)
	require.Zero(t, issue(email, replacement))
	redisClock.FastForward(ttl - time.Second)
	require.NoError(t, cache.MarkDelivered(ctx, email, replacement.ID, replacement.Generation))
	remaining, err := cache.ChallengeRemaining(ctx, email, replacement.ID, replacement.Generation)
	require.NoError(t, err)
	require.Equal(t, time.Second, remaining, "delivery must not extend expiry")
	redisClock.FastForward(time.Second)
	require.Nil(t, consume(replacement.ID, replacement.Generation, replacement.CodeMAC))
	old, err := legacy.GetVerificationCode(ctx, email)
	require.NoError(t, err)
	require.Equal(t, "123456", old.Code)
}

func TestEmailOTPCacheAtomicClaims(t *testing.T) {
	_, redisClock, rdb := newMiniredisEmailCache(t)
	cache := NewEmailOTPCache(rdb)
	ctx := context.Background()
	const email = "user@example.com"
	challenge := service.EmailOTPChallenge{
		ID: "first", Generation: "gen", CodeMAC: "fixed-mac",
		Subject: service.EmailOTPSubject{UserID: math.MaxInt64, AccountVersion: "snapshot"},
	}
	grant := service.EmailOTPRegistration{Email: email, Generation: challenge.Generation}
	const tokenHash = "token-hash"
	require.NoError(t, cache.StoreRegistration(ctx, tokenHash, grant, service.EmailOTPRegistrationTTL))
	got, err := cache.GetRegistration(ctx, tokenHash)
	require.NoError(t, err)
	require.Equal(t, grant, *got)

	var issuers atomic.Int32
	var calls sync.WaitGroup
	for range 20 {
		calls.Go(func() {
			wait, err := cache.Issue(ctx, email, challenge, service.EmailOTPCodeTTL, service.EmailOTPResendCooldown)
			if err != nil {
				t.Error(err)
			} else if wait == 0 {
				issuers.Add(1)
			}
		})
	}
	calls.Wait()
	require.EqualValues(t, 1, issuers.Load())
	require.NoError(t, cache.MarkDelivered(ctx, email, challenge.ID, challenge.Generation))
	var otpClaims, grantClaims atomic.Int32
	for range 20 {
		calls.Go(func() {
			subject, err := cache.Consume(ctx, email, challenge.ID, challenge.Generation, challenge.CodeMAC, service.EmailOTPMaxWrongAttempts)
			if err != nil {
				t.Error(err)
			} else if subject != nil {
				if *subject != challenge.Subject {
					t.Errorf("wrong subject: %+v", subject)
				}
				otpClaims.Add(1)
			}
			ok, err := cache.ConsumeRegistration(ctx, tokenHash)
			if err != nil {
				t.Error(err)
			} else if ok {
				grantClaims.Add(1)
			}
		})
	}
	calls.Wait()
	require.EqualValues(t, 1, otpClaims.Load())
	require.EqualValues(t, 1, grantClaims.Load())

	require.NoError(t, cache.StoreRegistration(ctx, tokenHash, grant, service.EmailOTPRegistrationTTL))
	redisClock.FastForward(service.EmailOTPRegistrationTTL)
	ok, err := cache.ConsumeRegistration(ctx, tokenHash)
	require.NoError(t, err)
	require.False(t, ok)
}
