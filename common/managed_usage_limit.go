package common

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	managedUsageWindowPrefix    = "caoliao:managed-token"
	managedUsageTwoHourDuration = 2 * time.Hour
)

var managedUsageLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type ManagedUsageLimitKind string

const (
	ManagedUsageLimitNone        ManagedUsageLimitKind = ""
	ManagedUsageLimitRequests2H  ManagedUsageLimitKind = "requests_2h"
	ManagedUsageLimitTokens2H    ManagedUsageLimitKind = "tokens_2h"
	ManagedUsageLimitTokensDaily ManagedUsageLimitKind = "tokens_daily"
)

type ManagedUsageReservation struct {
	TwoHourKey string
	DailyKey   string
	Reserved   int
}

type ManagedUsageLimitResult struct {
	Allowed     bool
	RetryAfter  int64
	Exceeded    ManagedUsageLimitKind
	Reservation *ManagedUsageReservation
}

type managedUsageMemoryWindow struct {
	used      int64
	expiresAt time.Time
}

var (
	managedUsageMemoryMu      sync.Mutex
	managedUsageMemoryWindows = make(map[string]managedUsageMemoryWindow)
)

const managedUsageSingleRedisScript = `
local window_ttl = tonumber(ARGV[3])
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('SET', KEYS[1], '1', 'EX', window_ttl)
end
local remaining = redis.call('TTL', KEYS[1])
if remaining < 1 then
  redis.call('SET', KEYS[1], '1', 'EX', window_ttl)
  remaining = window_ttl
end
local current = tonumber(redis.call('GET', KEYS[2]) or '0')
local requested = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
if current + requested > limit then
  return {0, remaining}
end
local updated = redis.call('INCRBY', KEYS[2], requested)
if updated == requested then redis.call('EXPIRE', KEYS[2], remaining) end
return {1, remaining}`

const managedUsageTokenRedisScript = `
local requested = tonumber(ARGV[1])
local two_hour_limit = tonumber(ARGV[2])
local daily_limit = tonumber(ARGV[3])
local window_ttl = tonumber(ARGV[4])
local daily_ttl = tonumber(ARGV[5])
local two_hour_ttl = window_ttl
if two_hour_limit > 0 then
  if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], '1', 'EX', window_ttl)
  end
  two_hour_ttl = redis.call('TTL', KEYS[1])
  if two_hour_ttl < 1 then
    redis.call('SET', KEYS[1], '1', 'EX', window_ttl)
    two_hour_ttl = window_ttl
  end
end
local two_hour_current = tonumber(redis.call('GET', KEYS[2]) or '0')
local daily_current = tonumber(redis.call('GET', KEYS[3]) or '0')
if two_hour_limit > 0 and two_hour_current + requested > two_hour_limit then
  return {0, 1, two_hour_ttl}
end
if daily_limit > 0 and daily_current + requested > daily_limit then
  local remaining = redis.call('TTL', KEYS[3])
  if remaining < 1 then remaining = daily_ttl end
  return {0, 2, remaining}
end
if two_hour_limit > 0 then
  local updated = redis.call('INCRBY', KEYS[2], requested)
  if updated == requested then redis.call('EXPIRE', KEYS[2], two_hour_ttl) end
end
if daily_limit > 0 then
  local updated = redis.call('INCRBY', KEYS[3], requested)
  if updated == requested then redis.call('EXPIRE', KEYS[3], daily_ttl) end
end
return {1, 0, 0}`

const managedUsageAdjustRedisScript = `
local delta = tonumber(ARGV[1])
for index = 1, #KEYS do
  if redis.call('EXISTS', KEYS[index]) == 1 then
    local current = tonumber(redis.call('GET', KEYS[index]) or '0')
    local updated = current + delta
    if updated < 0 then updated = 0 end
    local ttl = redis.call('TTL', KEYS[index])
    redis.call('SET', KEYS[index], updated)
    if ttl > 0 then redis.call('EXPIRE', KEYS[index], ttl) end
  end
end
return 1`

func ReserveManagedRequestLimit(ctx context.Context, tokenID int, limit int, now time.Time) (ManagedUsageLimitResult, error) {
	if limit <= 0 {
		return ManagedUsageLimitResult{Allowed: true}, nil
	}
	windowKey := fmt.Sprintf("%s:window-2h:%d", managedUsageWindowPrefix, tokenID)
	requestKey := fmt.Sprintf("%s:requests-2h:%d", managedUsageWindowPrefix, tokenID)
	windowTTL := int64(managedUsageTwoHourDuration / time.Second)
	if RedisEnabled {
		if RDB == nil {
			return ManagedUsageLimitResult{}, fmt.Errorf("redis is enabled but unavailable")
		}
		result, err := RDB.Eval(ctx, managedUsageSingleRedisScript, []string{windowKey, requestKey}, 1, limit, windowTTL).Result()
		if err != nil {
			return ManagedUsageLimitResult{}, err
		}
		values, err := managedUsageRedisValues(result, 2)
		if err != nil {
			return ManagedUsageLimitResult{}, err
		}
		return ManagedUsageLimitResult{Allowed: values[0] == 1, RetryAfter: values[1], Exceeded: ManagedUsageLimitRequests2H}, nil
	}
	managedUsageMemoryMu.Lock()
	defer managedUsageMemoryMu.Unlock()
	pruneManagedMemoryWindows(now)
	end := managedMemoryTwoHourEnd(windowKey, now)
	if managedMemoryWouldExceed(requestKey, limit, 1) {
		return ManagedUsageLimitResult{Allowed: false, RetryAfter: managedRetryAfter(now, end), Exceeded: ManagedUsageLimitRequests2H}, nil
	}
	addManagedMemoryUsage(requestKey, 1, end)
	return ManagedUsageLimitResult{Allowed: true, RetryAfter: managedRetryAfter(now, end), Exceeded: ManagedUsageLimitRequests2H}, nil
}

func ReserveManagedTokenLimits(ctx context.Context, tokenID int, twoHourLimit int, dailyLimit int, requested int, now time.Time) (ManagedUsageLimitResult, error) {
	if requested <= 0 || (twoHourLimit <= 0 && dailyLimit <= 0) {
		return ManagedUsageLimitResult{Allowed: true}, nil
	}
	_, dayEnd, dayBucket := managedDailyWindow(now)
	windowKey := fmt.Sprintf("%s:window-2h:%d", managedUsageWindowPrefix, tokenID)
	twoHourKey := fmt.Sprintf("%s:tokens-2h:%d", managedUsageWindowPrefix, tokenID)
	dailyKey := fmt.Sprintf("%s:tokens-day:%d:%s", managedUsageWindowPrefix, tokenID, dayBucket)
	windowTTL := int64(managedUsageTwoHourDuration / time.Second)
	dailyTTL := managedWindowTTL(now, dayEnd)
	reservation := &ManagedUsageReservation{Reserved: requested}
	if twoHourLimit > 0 {
		reservation.TwoHourKey = twoHourKey
	}
	if dailyLimit > 0 {
		reservation.DailyKey = dailyKey
	}

	if RedisEnabled {
		if RDB == nil {
			return ManagedUsageLimitResult{}, fmt.Errorf("redis is enabled but unavailable")
		}
		result, err := RDB.Eval(ctx, managedUsageTokenRedisScript, []string{windowKey, twoHourKey, dailyKey}, requested, twoHourLimit, dailyLimit, windowTTL, dailyTTL).Result()
		if err != nil {
			return ManagedUsageLimitResult{}, err
		}
		values, err := managedUsageRedisValues(result, 3)
		if err != nil {
			return ManagedUsageLimitResult{}, err
		}
		if values[0] == 1 {
			return ManagedUsageLimitResult{Allowed: true, Reservation: reservation}, nil
		}
		exceeded := ManagedUsageLimitTokens2H
		if values[1] == 2 {
			exceeded = ManagedUsageLimitTokensDaily
		}
		return ManagedUsageLimitResult{Allowed: false, RetryAfter: values[2], Exceeded: exceeded}, nil
	}

	managedUsageMemoryMu.Lock()
	defer managedUsageMemoryMu.Unlock()
	pruneManagedMemoryWindows(now)
	var twoHourEnd time.Time
	if twoHourLimit > 0 {
		twoHourEnd = managedMemoryTwoHourEnd(windowKey, now)
	}
	if twoHourLimit > 0 && managedMemoryWouldExceed(twoHourKey, twoHourLimit, requested) {
		return ManagedUsageLimitResult{Allowed: false, RetryAfter: managedRetryAfter(now, twoHourEnd), Exceeded: ManagedUsageLimitTokens2H}, nil
	}
	if dailyLimit > 0 && managedMemoryWouldExceed(dailyKey, dailyLimit, requested) {
		return ManagedUsageLimitResult{Allowed: false, RetryAfter: managedRetryAfter(now, dayEnd), Exceeded: ManagedUsageLimitTokensDaily}, nil
	}
	if twoHourLimit > 0 {
		addManagedMemoryUsage(twoHourKey, requested, twoHourEnd)
	}
	if dailyLimit > 0 {
		addManagedMemoryUsage(dailyKey, requested, dayEnd)
	}
	return ManagedUsageLimitResult{Allowed: true, Reservation: reservation}, nil
}

func ReconcileManagedTokenLimits(ctx context.Context, reservation *ManagedUsageReservation, actual int) error {
	if reservation == nil || reservation.Reserved <= 0 {
		return nil
	}
	if actual < 0 {
		actual = 0
	}
	delta := actual - reservation.Reserved
	if delta == 0 {
		return nil
	}
	keys := make([]string, 0, 2)
	if reservation.TwoHourKey != "" {
		keys = append(keys, reservation.TwoHourKey)
	}
	if reservation.DailyKey != "" {
		keys = append(keys, reservation.DailyKey)
	}
	if len(keys) == 0 {
		return nil
	}
	if RedisEnabled {
		if RDB == nil {
			return fmt.Errorf("redis is enabled but unavailable")
		}
		return RDB.Eval(ctx, managedUsageAdjustRedisScript, keys, delta).Err()
	}
	managedUsageMemoryMu.Lock()
	defer managedUsageMemoryMu.Unlock()
	for _, key := range keys {
		window, ok := managedUsageMemoryWindows[key]
		if !ok {
			continue
		}
		window.used += int64(delta)
		if window.used < 0 {
			window.used = 0
		}
		managedUsageMemoryWindows[key] = window
	}
	return nil
}

func managedDailyWindow(now time.Time) (time.Time, time.Time, string) {
	local := now.In(managedUsageLocation)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, managedUsageLocation)
	return start, start.AddDate(0, 0, 1), start.Format("20060102")
}

func managedWindowTTL(now time.Time, end time.Time) int64 {
	ttl := int64(math.Ceil(end.Sub(now).Seconds()))
	if ttl < 1 {
		return 1
	}
	return ttl
}

func managedRetryAfter(now time.Time, end time.Time) int64 {
	retry := int64(math.Ceil(end.Sub(now).Seconds()))
	if retry < 1 {
		return 1
	}
	return retry
}

func managedMemoryTwoHourEnd(windowKey string, now time.Time) time.Time {
	window, ok := managedUsageMemoryWindows[windowKey]
	if ok && now.Before(window.expiresAt) {
		return window.expiresAt
	}
	end := now.Add(managedUsageTwoHourDuration)
	managedUsageMemoryWindows[windowKey] = managedUsageMemoryWindow{expiresAt: end}
	return end
}

func managedMemoryWouldExceed(key string, limit int, requested int) bool {
	return managedUsageMemoryWindows[key].used+int64(requested) > int64(limit)
}

func addManagedMemoryUsage(key string, requested int, end time.Time) {
	window := managedUsageMemoryWindows[key]
	window.used += int64(requested)
	window.expiresAt = end
	managedUsageMemoryWindows[key] = window
}

func pruneManagedMemoryWindows(now time.Time) {
	for key, window := range managedUsageMemoryWindows {
		if !now.Before(window.expiresAt) {
			delete(managedUsageMemoryWindows, key)
		}
	}
}

func managedUsageRedisValues(result interface{}, expected int) ([]int64, error) {
	items, ok := result.([]interface{})
	if !ok || len(items) != expected {
		return nil, fmt.Errorf("unexpected redis rate-limit result: %T", result)
	}
	values := make([]int64, expected)
	for index, item := range items {
		switch typed := item.(type) {
		case int64:
			values[index] = typed
		case string:
			var parsed int64
			if _, err := fmt.Sscan(typed, &parsed); err != nil {
				return nil, err
			}
			values[index] = parsed
		case []byte:
			var parsed int64
			if _, err := fmt.Sscan(string(typed), &parsed); err != nil {
				return nil, err
			}
			values[index] = parsed
		default:
			return nil, fmt.Errorf("unexpected redis integer type: %T", item)
		}
	}
	return values, nil
}
