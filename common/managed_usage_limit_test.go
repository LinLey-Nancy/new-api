package common

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetManagedUsageMemoryForTest() {
	managedUsageMemoryMu.Lock()
	defer managedUsageMemoryMu.Unlock()
	managedUsageMemoryWindows = make(map[string]managedUsageMemoryWindow)
}

func TestManagedDailyWindowUsesBeijingMidnightBoundary(t *testing.T) {
	beforeMidnight := time.Date(2026, 9, 1, 15, 59, 59, 0, time.UTC) // 23:59:59 Beijing
	dayStart, dayEnd, dayBucket := managedDailyWindow(beforeMidnight)

	assert.Equal(t, "20260901", dayBucket)
	assert.Equal(t, 1, dayStart.In(managedUsageLocation).Day())
	assert.Equal(t, 2, dayEnd.In(managedUsageLocation).Day())

	afterMidnight := beforeMidnight.Add(time.Second)
	_, _, nextDayBucket := managedDailyWindow(afterMidnight)
	assert.Equal(t, "20260902", nextDayBucket)

	boundary := time.Date(2026, 9, 2, 0, 0, 0, 0, managedUsageLocation)
	assert.EqualValues(t, 2, managedWindowTTL(boundary.Add(-1500*time.Millisecond), boundary),
		"Redis TTL must round up so a quota cannot reset before Beijing midnight")
	assert.EqualValues(t, 2, managedRetryAfter(boundary.Add(-1500*time.Millisecond), boundary))
}

func TestManagedTwoHourWindowStartsOnFirstUseAndIsSharedByKeyLimits(t *testing.T) {
	previousRedisEnabled := RedisEnabled
	RedisEnabled = false
	resetManagedUsageMemoryForTest()
	t.Cleanup(func() {
		RedisEnabled = previousRedisEnabled
		resetManagedUsageMemoryForTest()
	})

	ctx := context.Background()
	firstUse := time.Date(2026, 9, 1, 13, 59, 0, 0, managedUsageLocation)
	firstRequest, err := ReserveManagedRequestLimit(ctx, 401, 1, firstUse)
	require.NoError(t, err)
	require.True(t, firstRequest.Allowed)

	afterClockBoundary, err := ReserveManagedRequestLimit(ctx, 401, 1, firstUse.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, afterClockBoundary.Allowed, "an even-hour clock boundary must not reset the key window")
	assert.EqualValues(t, 7_140, afterClockBoundary.RetryAfter)

	outputNearEnd, err := ReserveManagedTokenLimits(ctx, 401, 100, 0, 100, firstUse.Add(90*time.Minute))
	require.NoError(t, err)
	require.True(t, outputNearEnd.Allowed)
	require.NoError(t, ReconcileManagedTokenLimits(ctx, outputNearEnd.Reservation, 100))

	reset, err := ReserveManagedTokenLimits(ctx, 401, 100, 0, 100, firstUse.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, reset.Allowed, "request and output limits must reset together two hours after the key's first use")
}

func TestManagedTwoHourWindowUsesSharedRedisAnchor(t *testing.T) {
	redisServer := useManagedUsageTestRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 13, 59, 0, 0, managedUsageLocation)
	first, err := ReserveManagedRequestLimit(ctx, 402, 1, now)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	redisServer.FastForward(90 * time.Minute)
	output, err := ReserveManagedTokenLimits(ctx, 402, 100, 0, 100, now.Add(90*time.Minute))
	require.NoError(t, err)
	require.True(t, output.Allowed)

	redisServer.FastForward(30 * time.Minute)
	reset, err := ReserveManagedTokenLimits(ctx, 402, 100, 0, 100, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, reset.Allowed, "the Redis output counter must expire with the request-created key window")
}

func useManagedUsageTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	previousRedisEnabled := RedisEnabled
	previousRedisClient := RDB
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	RedisEnabled = true
	RDB = redisClient
	t.Cleanup(func() {
		_ = redisClient.Close()
		RedisEnabled = previousRedisEnabled
		RDB = previousRedisClient
	})
	return redisServer
}

func TestManagedRedisReconcilePreservesSubsecondExpiry(t *testing.T) {
	redisServer := useManagedUsageTestRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 22, 0, 0, 0, managedUsageLocation)
	first, err := ReserveManagedTokenLimits(ctx, 406, 100, 100, 100, now)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	const remaining = 250 * time.Millisecond
	redisServer.FastForward(2*time.Hour - remaining)
	require.NoError(t, ReconcileManagedTokenLimits(ctx, first.Reservation, 80))
	for _, key := range []string{first.Reservation.TwoHourKey, first.Reservation.DailyKey} {
		assert.Equal(t, remaining, redisServer.TTL(key))
		assert.Equal(t, "80", RDB.Get(ctx, key).Val())
	}
	blocked, err := ReserveManagedTokenLimits(ctx, 406, 100, 100, 21, now.Add(2*time.Hour-remaining))
	require.NoError(t, err)
	assert.False(t, blocked.Allowed)
	assert.Equal(t, ManagedUsageLimitTokens2H, blocked.Exceeded)
	assert.EqualValues(t, 1, blocked.RetryAfter)

	redisServer.FastForward(remaining)
	assert.False(t, redisServer.Exists(first.Reservation.TwoHourKey))
	assert.False(t, redisServer.Exists(first.Reservation.DailyKey))
}

func TestManagedRedisReservationsDiscardLegacyTwoHourCounters(t *testing.T) {
	for _, duration := range []time.Duration{0, 30 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			redisServer := useManagedUsageTestRedis(t)
			ctx := context.Background()
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, managedUsageLocation)
			const (
				windowKey  = "caoliao:managed-token:window-2h:407"
				requestKey = "caoliao:managed-token:requests-2h:407"
				outputKey  = "caoliao:managed-token:tokens-2h:407"
			)
			if duration > 0 {
				require.NoError(t, RDB.Set(ctx, windowKey, 1, duration).Err())
			}
			require.NoError(t, RDB.Set(ctx, requestKey, 20, 0).Err())
			var outputTTL time.Duration
			if duration == 0 {
				// Older scripts could leave an expiring counter after the anchor expired.
				outputTTL = time.Second
			}
			require.NoError(t, RDB.Set(ctx, outputKey, 976_121, outputTTL).Err())

			request, err := ReserveManagedRequestLimit(ctx, 407, 1, now)
			require.NoError(t, err)
			assert.True(t, request.Allowed)
			output, err := ReserveManagedTokenLimits(ctx, 407, 1_000_000, 0, 65_535, now)
			require.NoError(t, err)
			assert.True(t, output.Allowed)
			wantTTL := duration
			if wantTTL == 0 {
				wantTTL = 2 * time.Hour
			}
			assert.Equal(t, wantTTL, redisServer.TTL(windowKey))
			assert.Equal(t, wantTTL, redisServer.TTL(requestKey))
			assert.Equal(t, wantTTL, redisServer.TTL(outputKey))
			assert.Equal(t, "1", RDB.Get(ctx, requestKey).Val())
			assert.Equal(t, "65535", RDB.Get(ctx, outputKey).Val())
		})
	}
}

func TestManagedRedisReconcileDiscardsLegacyPermanentTwoHourCounter(t *testing.T) {
	redisServer := useManagedUsageTestRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, managedUsageLocation)
	result, err := ReserveManagedTokenLimits(ctx, 408, 100, 0, 100, now)
	require.NoError(t, err)
	require.True(t, result.Allowed)
	key := result.Reservation.TwoHourKey
	require.NoError(t, RDB.Persist(ctx, key).Err())

	require.NoError(t, ReconcileManagedTokenLimits(ctx, result.Reservation, 50))
	assert.False(t, redisServer.Exists(key))
}

func TestManagedRedisReservationRestoresLegacyDailyExpiry(t *testing.T) {
	redisServer := useManagedUsageTestRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, managedUsageLocation)
	const key = "caoliao:managed-token:tokens-day:409:20260901"
	require.NoError(t, RDB.Set(ctx, key, 95, 0).Err())

	result, err := ReserveManagedTokenLimits(ctx, 409, 0, 100, 10, now)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, ManagedUsageLimitTokensDaily, result.Exceeded)
	assert.Equal(t, "95", RDB.Get(ctx, key).Val())
	assert.Equal(t, 12*time.Hour, redisServer.TTL(key))
}

func TestManagedRedisReconcileRestoresLegacyDailyExpiry(t *testing.T) {
	redisServer := useManagedUsageTestRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, managedUsageLocation)
	redisServer.SetTime(now)
	result, err := ReserveManagedTokenLimits(ctx, 410, 0, 100, 100, now)
	require.NoError(t, err)
	require.True(t, result.Allowed)
	key := result.Reservation.DailyKey
	require.NoError(t, RDB.Persist(ctx, key).Err())

	require.NoError(t, ReconcileManagedTokenLimits(ctx, result.Reservation, 70))
	assert.Equal(t, "70", RDB.Get(ctx, key).Val())
	assert.Equal(t, 12*time.Hour, redisServer.TTL(key))
}

func TestManagedTwoHourWindowSharesOutputCreatedRedisAnchorWithRequests(t *testing.T) {
	previousRedisEnabled := RedisEnabled
	previousRedisClient := RDB
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	RedisEnabled = true
	RDB = redisClient
	t.Cleanup(func() {
		_ = redisClient.Close()
		RedisEnabled = previousRedisEnabled
		RDB = previousRedisClient
	})

	ctx := context.Background()
	now := time.Date(2026, 9, 1, 13, 59, 0, 0, managedUsageLocation)
	output, err := ReserveManagedTokenLimits(ctx, 403, 100, 0, 100, now)
	require.NoError(t, err)
	require.True(t, output.Allowed)

	redisServer.FastForward(90 * time.Minute)
	firstRequest, err := ReserveManagedRequestLimit(ctx, 403, 1, now.Add(90*time.Minute))
	require.NoError(t, err)
	require.True(t, firstRequest.Allowed)
	blocked, err := ReserveManagedRequestLimit(ctx, 403, 1, now.Add(90*time.Minute))
	require.NoError(t, err)
	assert.False(t, blocked.Allowed)
	assert.EqualValues(t, 30*time.Minute/time.Second, blocked.RetryAfter)

	redisServer.FastForward(30 * time.Minute)
	reset, err := ReserveManagedRequestLimit(ctx, 403, 1, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, reset.Allowed, "request counters must expire with an output-created key window")
}

func TestManagedRequestMemoryFallbackIsAtomicOnFirstUse(t *testing.T) {
	previousRedisEnabled := RedisEnabled
	RedisEnabled = false
	resetManagedUsageMemoryForTest()
	t.Cleanup(func() {
		RedisEnabled = previousRedisEnabled
		resetManagedUsageMemoryForTest()
	})

	const (
		workers = 40
		limit   = 7
	)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 13, 59, 0, 0, managedUsageLocation)
	start := make(chan struct{})
	results := make(chan ManagedUsageLimitResult, workers)
	errorsFound := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := ReserveManagedRequestLimit(ctx, 404, limit, now)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsFound)
	require.Empty(t, errorsFound)

	allowed := 0
	for result := range results {
		if result.Allowed {
			allowed++
		} else {
			assert.EqualValues(t, 2*time.Hour/time.Second, result.RetryAfter)
		}
	}
	assert.Equal(t, limit, allowed, "concurrent first use must reserve exactly the configured request count")
}

func TestManagedRequestRedisFallbackIsAtomicOnFirstUse(t *testing.T) {
	previousRedisEnabled := RedisEnabled
	previousRedisClient := RDB
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	RedisEnabled = true
	RDB = redisClient
	t.Cleanup(func() {
		_ = redisClient.Close()
		RedisEnabled = previousRedisEnabled
		RDB = previousRedisClient
	})

	const (
		workers = 40
		limit   = 7
	)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 13, 59, 0, 0, managedUsageLocation)
	start := make(chan struct{})
	results := make(chan ManagedUsageLimitResult, workers)
	errorsFound := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := ReserveManagedRequestLimit(ctx, 405, limit, now)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsFound)
	require.Empty(t, errorsFound)

	allowed := 0
	for result := range results {
		if result.Allowed {
			allowed++
		} else {
			assert.EqualValues(t, 2*time.Hour/time.Second, result.RetryAfter)
		}
	}
	assert.Equal(t, limit, allowed, "the Redis script must atomically create one window and enforce its request limit")
}

func TestManagedOutputLimitsReconcileToActualOutputAndResetDailyAtMidnight(t *testing.T) {
	previousRedisEnabled := RedisEnabled
	RedisEnabled = false
	resetManagedUsageMemoryForTest()
	t.Cleanup(func() {
		RedisEnabled = previousRedisEnabled
		resetManagedUsageMemoryForTest()
	})

	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 30, 0, 0, managedUsageLocation)
	first, err := ReserveManagedTokenLimits(ctx, 501, 100, 120, 80, now)
	require.NoError(t, err)
	require.True(t, first.Allowed)
	require.NotNil(t, first.Reservation)
	require.NoError(t, ReconcileManagedTokenLimits(ctx, first.Reservation, 30))

	second, err := ReserveManagedTokenLimits(ctx, 501, 100, 120, 71, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, second.Allowed)
	assert.Equal(t, ManagedUsageLimitTokens2H, second.Exceeded)

	third, err := ReserveManagedTokenLimits(ctx, 501, 100, 120, 70, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.True(t, third.Allowed, "a new two-hour window must allow another reservation")
	require.NoError(t, ReconcileManagedTokenLimits(ctx, third.Reservation, 70))

	dailyBlocked, err := ReserveManagedTokenLimits(ctx, 501, 100, 120, 21, now.Add(2*time.Hour+time.Minute))
	require.NoError(t, err)
	assert.False(t, dailyBlocked.Allowed)
	assert.Equal(t, ManagedUsageLimitTokensDaily, dailyBlocked.Exceeded)

	nextDay := time.Date(2026, 9, 2, 0, 0, 0, 0, managedUsageLocation)
	reset, err := ReserveManagedTokenLimits(ctx, 501, 100, 120, 100, nextDay)
	require.NoError(t, err)
	assert.True(t, reset.Allowed, "daily output limit must reset at Beijing midnight")
}
