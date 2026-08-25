package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	caoliaoManagedBy               = "caoliao"
	managedTokenWindowDuration     = time.Minute
	defaultManagedOutputTokenLimit = 4096
)

type managedTokenWindow struct {
	startedAt time.Time
	used      int64
}

var (
	managedTokenWindowMu sync.Mutex
	managedTokenWindows  = make(map[string]managedTokenWindow)
)

const managedTokenRedisScript = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local requested = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
if current + requested > limit then
  local remaining = redis.call('TTL', KEYS[1])
  if remaining < 1 then remaining = ttl end
  return {0, remaining}
end
local updated = redis.call('INCRBY', KEYS[1], requested)
if updated == requested then redis.call('EXPIRE', KEYS[1], ttl) end
local remaining = redis.call('TTL', KEYS[1])
if remaining < 1 then remaining = ttl end
return {1, remaining}`

func managedOutputTokenReserve() int {
	configured := strings.TrimSpace(os.Getenv("CAOLIAO_DEFAULT_MAX_OUTPUT_TOKENS"))
	if configured == "" {
		return defaultManagedOutputTokenLimit
	}
	value, err := strconv.Atoi(configured)
	if err != nil || value < 0 {
		return defaultManagedOutputTokenLimit
	}
	return value
}

func takeManagedTokenLimit(ctx context.Context, key string, limit int, requested int) (bool, int64, error) {
	if limit <= 0 || requested <= 0 {
		return true, 0, nil
	}
	if requested > limit {
		return false, int64(managedTokenWindowDuration.Seconds()), nil
	}

	if common.RedisEnabled {
		if common.RDB == nil {
			return false, 0, errors.New("redis is enabled but unavailable")
		}
		result, err := common.RDB.Eval(ctx, managedTokenRedisScript, []string{key}, requested, limit, int(managedTokenWindowDuration.Seconds())).Result()
		if err != nil {
			return false, 0, err
		}
		values, ok := result.([]interface{})
		if !ok || len(values) != 2 {
			return false, 0, fmt.Errorf("unexpected redis rate-limit result: %T", result)
		}
		allowed, err := redisResultInt64(values[0])
		if err != nil {
			return false, 0, err
		}
		retryAfter, err := redisResultInt64(values[1])
		if err != nil {
			return false, 0, err
		}
		return allowed == 1, retryAfter, nil
	}

	now := time.Now()
	managedTokenWindowMu.Lock()
	defer managedTokenWindowMu.Unlock()

	window, exists := managedTokenWindows[key]
	if !exists || now.Sub(window.startedAt) >= managedTokenWindowDuration {
		window = managedTokenWindow{startedAt: now}
	}
	retryAfter := int64(time.Until(window.startedAt.Add(managedTokenWindowDuration)).Seconds()) + 1
	if retryAfter < 1 {
		retryAfter = 1
	}
	if window.used+int64(requested) > int64(limit) {
		managedTokenWindows[key] = window
		return false, retryAfter, nil
	}
	window.used += int64(requested)
	managedTokenWindows[key] = window

	if len(managedTokenWindows) > 10000 {
		for itemKey, item := range managedTokenWindows {
			if now.Sub(item.startedAt) >= 2*managedTokenWindowDuration {
				delete(managedTokenWindows, itemKey)
			}
		}
	}
	return true, retryAfter, nil
}

func redisResultInt64(value interface{}) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected redis integer type: %T", value)
	}
}

func managedTokenLimitKey(c *gin.Context, kind string) string {
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	return fmt.Sprintf("caoliao:managed-token:%s:%d", kind, tokenID)
}

// EnforceManagedTokenRPM applies the per-key request limit only to keys created
// through the Caoliao integration. A zero limit means unlimited.
func EnforceManagedTokenRPM(c *gin.Context) bool {
	if common.GetContextKeyString(c, constant.ContextKeyTokenManagedBy) != caoliaoManagedBy {
		return true
	}
	limit := common.GetContextKeyInt(c, constant.ContextKeyTokenRPM)
	allowed, retryAfter, err := takeManagedTokenLimit(c.Request.Context(), managedTokenLimitKey(c, "rpm"), limit, 1)
	if err != nil {
		abortWithOpenAiMessage(c, http.StatusInternalServerError, "API key rate-limit check failed", types.ErrorCode("rate_limit_check_failed"))
		return false
	}
	if allowed {
		return true
	}
	c.Header("Retry-After", strconv.FormatInt(retryAfter, 10))
	model.RecordCaoliaoRateLimitLog(c, "rpm")
	abortWithOpenAiMessage(c, http.StatusTooManyRequests, fmt.Sprintf("API key request limit exceeded: at most %d requests per minute", limit), types.ErrorCode("rate_limit_exceeded"))
	return false
}

// ReserveManagedTokenTPM reserves the estimated prompt and maximum completion
// tokens for this request. Reserving before relay prevents concurrent requests
// from oversubscribing a key's per-minute budget.
func ReserveManagedTokenTPM(c *gin.Context, promptTokens int, maxOutputTokens int) (bool, int64, error) {
	if common.GetContextKeyString(c, constant.ContextKeyTokenManagedBy) != caoliaoManagedBy {
		return true, 0, nil
	}
	limit := common.GetContextKeyInt(c, constant.ContextKeyTokenTPM)
	if maxOutputTokens <= 0 {
		maxOutputTokens = managedOutputTokenReserve()
	}
	requested := ManagedTokenRequestReserve(promptTokens, maxOutputTokens)
	allowed, retryAfter, err := takeManagedTokenLimit(c.Request.Context(), managedTokenLimitKey(c, "tpm"), limit, requested)
	if err == nil && !allowed {
		model.RecordCaoliaoRateLimitLog(c, "tpm")
	}
	return allowed, retryAfter, err
}

// ManagedTokenRequestReserve is the shared worst-case reservation used by
// both the TPM window and the key's total token quota.
func ManagedTokenRequestReserve(promptTokens int, maxOutputTokens int) int {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = managedOutputTokenReserve()
	}
	if promptTokens > common.MaxQuota-maxOutputTokens {
		return common.MaxQuota
	}
	return promptTokens + maxOutputTokens
}
