package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedOutputTokenRequestReserveUsesOnlyDeclaredOrDefaultOutput(t *testing.T) {
	t.Setenv("CAOLIAO_DEFAULT_MAX_OUTPUT_TOKENS", "4096")

	assert.Equal(t, 500, ManagedOutputTokenRequestReserve(500))
	assert.Equal(t, 4_096, ManagedOutputTokenRequestReserve(0))
}

func TestEnforceManagedTokenRequestLimitReturnsOpenAI429(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-v4-flash"}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		common.SetContextKey(ctx, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)
		common.SetContextKey(ctx, constant.ContextKeyTokenId, 9_101)
		common.SetContextKey(ctx, constant.ContextKeyTokenRequestsPerTwoHours, 1)
		return ctx, recorder
	}

	first, _ := newContext()
	assert.True(t, EnforceManagedTokenRequestLimit(first))
	second, secondRecorder := newContext()
	assert.False(t, EnforceManagedTokenRequestLimit(second))
	assert.Equal(t, http.StatusTooManyRequests, secondRecorder.Code)
	assert.NotEmpty(t, secondRecorder.Header().Get("Retry-After"))
	assert.Contains(t, secondRecorder.Body.String(), `"code":"caoliao_requests_2h_limit_exceeded"`)
	assert.Contains(t, secondRecorder.Body.String(), "Two-hour dynamic request quota exhausted")
	assert.Equal(t, "deepseek-v4-flash",
		common.GetContextKeyString(second, constant.ContextKeyOriginalModel))

	ordinaryRecorder := httptest.NewRecorder()
	ordinary, _ := gin.CreateTestContext(ordinaryRecorder)
	ordinary.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	assert.True(t, EnforceManagedTokenRequestLimit(ordinary))
	assert.Equal(t, http.StatusOK, ordinaryRecorder.Code)
}

func TestReserveManagedOutputTokenLimitsUsesOutputOnlyAndPerKeyBudget(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	newContext := func(tokenID int) *gin.Context {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(ctx, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)
		common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
		common.SetContextKey(ctx, constant.ContextKeyTokenTokensPerTwoHours, 100)
		common.SetContextKey(ctx, constant.ContextKeyTokenDailyTokenQuota, 200)
		return ctx
	}

	first := newContext(9_201)
	result, err := ReserveManagedOutputTokenLimits(first, 60)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	value, ok := common.GetContextKey(first, constant.ContextKeyManagedUsageReservation)
	require.True(t, ok)
	reservation, ok := value.(*common.ManagedUsageReservation)
	require.True(t, ok)
	require.NoError(t, common.ReconcileManagedTokenLimits(first.Request.Context(), reservation, 20))

	second := newContext(9_201)
	result, err = ReserveManagedOutputTokenLimits(second, 81)
	require.NoError(t, err)
	assert.False(t, result.Allowed, "20 actual output + 81 reserved output must exceed the two-hour output limit")
	assert.Positive(t, result.RetryAfter)
	assert.Equal(t, common.ManagedUsageLimitTokens2H, result.Exceeded)

	otherKey := newContext(9_202)
	result, err = ReserveManagedOutputTokenLimits(otherKey, 100)
	require.NoError(t, err)
	assert.True(t, result.Allowed, "different managed keys must not share output-token counters")
}

func TestManagedUsageLimitErrorsExposeStableClientCodes(t *testing.T) {
	tests := []struct {
		kind    common.ManagedUsageLimitKind
		code    types.ErrorCode
		message string
	}{
		{common.ManagedUsageLimitRequests2H, CaoliaoRequestsTwoHourLimitCode, "Two-hour dynamic request quota"},
		{common.ManagedUsageLimitTokens2H, CaoliaoOutputTwoHourLimitCode, "Two-hour dynamic output token quota"},
		{common.ManagedUsageLimitTokensDaily, CaoliaoOutputDailyLimitCode, "Daily fixed output token quota"},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			code, message := ManagedUsageLimitError(test.kind, 321)
			assert.Equal(t, test.code, code)
			assert.Contains(t, message, test.message)
			assert.Contains(t, message, "321 seconds")
		})
	}
}
