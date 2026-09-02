package middleware

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	caoliaoManagedBy                = "caoliao"
	defaultManagedOutputTokenLimit  = 4096
	CaoliaoRequestsTwoHourLimitCode = types.ErrorCode("caoliao_requests_2h_limit_exceeded")
	CaoliaoOutputTwoHourLimitCode   = types.ErrorCode("caoliao_output_tokens_2h_limit_exceeded")
	CaoliaoOutputDailyLimitCode     = types.ErrorCode("caoliao_output_tokens_daily_limit_exceeded")
)

func ManagedUsageLimitError(kind common.ManagedUsageLimitKind, retryAfter int64) (types.ErrorCode, string) {
	switch kind {
	case common.ManagedUsageLimitRequests2H:
		return CaoliaoRequestsTwoHourLimitCode, fmt.Sprintf("Two-hour dynamic request quota exhausted; this API key window resets in %d seconds", retryAfter)
	case common.ManagedUsageLimitTokens2H:
		return CaoliaoOutputTwoHourLimitCode, fmt.Sprintf("Two-hour dynamic output token quota exhausted; this API key window resets in %d seconds", retryAfter)
	case common.ManagedUsageLimitTokensDaily:
		return CaoliaoOutputDailyLimitCode, fmt.Sprintf("Daily fixed output token quota exhausted; quota resets at 00:00 Asia/Shanghai in %d seconds", retryAfter)
	default:
		return types.ErrorCode("rate_limit_exceeded"), fmt.Sprintf("API key quota exhausted; retry in %d seconds", retryAfter)
	}
}

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

// EnforceManagedTokenRequestLimit applies the per-key request count to a
// two-hour window that starts on the key's first managed request. A zero limit
// means unlimited.
func EnforceManagedTokenRequestLimit(c *gin.Context) bool {
	if common.GetContextKeyString(c, constant.ContextKeyTokenManagedBy) != caoliaoManagedBy {
		return true
	}
	if common.GetContextKeyString(c, constant.ContextKeyOriginalModel) == "" {
		modelRequest, _, err := getModelRequest(c)
		if err == nil && modelRequest != nil && strings.TrimSpace(modelRequest.Model) != "" {
			common.SetContextKey(c, constant.ContextKeyOriginalModel, modelRequest.Model)
		}
	}
	limit := common.GetContextKeyInt(c, constant.ContextKeyTokenRequestsPerTwoHours)
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	result, err := common.ReserveManagedRequestLimit(c.Request.Context(), tokenID, limit, time.Now())
	if err != nil {
		abortWithOpenAiMessage(c, http.StatusInternalServerError, "API key rate-limit check failed", types.ErrorCode("rate_limit_check_failed"))
		return false
	}
	if result.Allowed {
		return true
	}
	c.Header("Retry-After", strconv.FormatInt(result.RetryAfter, 10))
	model.RecordCaoliaoRateLimitLog(c, string(common.ManagedUsageLimitRequests2H))
	code, message := ManagedUsageLimitError(result.Exceeded, result.RetryAfter)
	abortWithOpenAiMessage(c, http.StatusTooManyRequests, message, code)
	return false
}

// ReserveManagedOutputTokenLimits reserves only the maximum possible output
// tokens. Input/prompt tokens never count against the two-hour or daily limit.
func ReserveManagedOutputTokenLimits(c *gin.Context, maxOutputTokens int) (common.ManagedUsageLimitResult, error) {
	if common.GetContextKeyString(c, constant.ContextKeyTokenManagedBy) != caoliaoManagedBy {
		return common.ManagedUsageLimitResult{Allowed: true}, nil
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = managedOutputTokenReserve()
	}
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	twoHourLimit := common.GetContextKeyInt(c, constant.ContextKeyTokenTokensPerTwoHours)
	dailyLimit := common.GetContextKeyInt(c, constant.ContextKeyTokenDailyTokenQuota)
	result, err := common.ReserveManagedTokenLimits(c.Request.Context(), tokenID, twoHourLimit, dailyLimit, maxOutputTokens, time.Now())
	if err != nil {
		return common.ManagedUsageLimitResult{}, err
	}
	if result.Allowed && result.Reservation != nil {
		common.SetContextKey(c, constant.ContextKeyManagedUsageReservation, result.Reservation)
	}
	if !result.Allowed {
		model.RecordCaoliaoRateLimitLog(c, string(result.Exceeded))
	}
	return result, nil
}

// ManagedOutputTokenRequestReserve is the worst-case output-only reservation
// shared by rate limits and pre-consume protection.
func ManagedOutputTokenRequestReserve(maxOutputTokens int) int {
	if maxOutputTokens <= 0 {
		return managedOutputTokenReserve()
	}
	return maxOutputTokens
}
