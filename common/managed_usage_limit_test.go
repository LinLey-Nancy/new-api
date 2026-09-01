package common

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetManagedUsageMemoryForTest() {
	managedUsageMemoryMu.Lock()
	defer managedUsageMemoryMu.Unlock()
	managedUsageMemoryWindows = make(map[string]managedUsageMemoryWindow)
}

func TestManagedUsageWindowsUseBeijingTwoHourAndMidnightBoundaries(t *testing.T) {
	beforeMidnight := time.Date(2026, 9, 1, 15, 59, 59, 0, time.UTC) // 23:59:59 Beijing
	twoHourStart, twoHourEnd, twoHourBucket := managedTwoHourWindow(beforeMidnight)
	dayStart, dayEnd, dayBucket := managedDailyWindow(beforeMidnight)

	assert.Equal(t, "2026090122", twoHourBucket)
	assert.Equal(t, "20260901", dayBucket)
	assert.Equal(t, 22, twoHourStart.In(managedUsageLocation).Hour())
	assert.Equal(t, 0, twoHourEnd.In(managedUsageLocation).Hour())
	assert.Equal(t, 1, dayStart.In(managedUsageLocation).Day())
	assert.Equal(t, 2, dayEnd.In(managedUsageLocation).Day())

	afterMidnight := beforeMidnight.Add(time.Second)
	_, _, nextTwoHourBucket := managedTwoHourWindow(afterMidnight)
	_, _, nextDayBucket := managedDailyWindow(afterMidnight)
	assert.Equal(t, "2026090200", nextTwoHourBucket)
	assert.Equal(t, "20260902", nextDayBucket)

	boundary := time.Date(2026, 9, 2, 0, 0, 0, 0, managedUsageLocation)
	assert.EqualValues(t, 2, managedWindowTTL(boundary.Add(-1500*time.Millisecond), boundary),
		"Redis TTL must round up so a quota cannot reset before Beijing midnight")
	assert.EqualValues(t, 2, managedRetryAfter(boundary.Add(-1500*time.Millisecond), boundary))
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
