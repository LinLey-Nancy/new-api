package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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
	assert.Contains(t, secondRecorder.Body.String(), `"code":"rate_limit_exceeded"`)
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
	allowed, _, err := ReserveManagedOutputTokenLimits(first, 60)
	require.NoError(t, err)
	assert.True(t, allowed)
	value, ok := common.GetContextKey(first, constant.ContextKeyManagedUsageReservation)
	require.True(t, ok)
	reservation, ok := value.(*common.ManagedUsageReservation)
	require.True(t, ok)
	require.NoError(t, common.ReconcileManagedTokenLimits(first.Request.Context(), reservation, 20))

	second := newContext(9_201)
	allowed, retryAfter, err := ReserveManagedOutputTokenLimits(second, 81)
	require.NoError(t, err)
	assert.False(t, allowed, "20 actual output + 81 reserved output must exceed the two-hour output limit")
	assert.Positive(t, retryAfter)

	otherKey := newContext(9_202)
	allowed, _, err = ReserveManagedOutputTokenLimits(otherKey, 100)
	require.NoError(t, err)
	assert.True(t, allowed, "different managed keys must not share output-token counters")
}
