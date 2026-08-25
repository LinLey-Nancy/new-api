package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedTokenRequestReserveUsesDeclaredOrDefaultOutput(t *testing.T) {
	t.Setenv("CAOLIAO_DEFAULT_MAX_OUTPUT_TOKENS", "4096")

	assert.Equal(t, 600, ManagedTokenRequestReserve(100, 500))
	assert.Equal(t, 4_196, ManagedTokenRequestReserve(100, 0))
	assert.Equal(t, common.MaxQuota, ManagedTokenRequestReserve(common.MaxQuota-5, 10))
}

func resetManagedTokenWindowsForTest() {
	managedTokenWindowMu.Lock()
	defer managedTokenWindowMu.Unlock()
	managedTokenWindows = make(map[string]managedTokenWindow)
}

func TestTakeManagedTokenLimitCountsRequests(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = previousRedisEnabled
		resetManagedTokenWindowsForTest()
	})
	resetManagedTokenWindowsForTest()

	allowed, _, err := takeManagedTokenLimit(context.Background(), "test:rpm", 2, 1)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, _, err = takeManagedTokenLimit(context.Background(), "test:rpm", 2, 1)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, retryAfter, err := takeManagedTokenLimit(context.Background(), "test:rpm", 2, 1)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Positive(t, retryAfter)
}

func TestTakeManagedTokenLimitUsesWeightedBudget(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = previousRedisEnabled
		resetManagedTokenWindowsForTest()
	})
	resetManagedTokenWindowsForTest()

	allowed, _, err := takeManagedTokenLimit(context.Background(), "test:tpm", 100, 60)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, _, err = takeManagedTokenLimit(context.Background(), "test:tpm", 100, 41)
	require.NoError(t, err)
	assert.False(t, allowed)
	allowed, _, err = takeManagedTokenLimit(context.Background(), "test:tpm", 100, 40)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestTakeManagedTokenLimitZeroMeansUnlimited(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	allowed, retryAfter, err := takeManagedTokenLimit(context.Background(), "test:unlimited", 0, 1_000_000)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Zero(t, retryAfter)
}

func TestTakeManagedTokenLimitUsesRedisFixedWindow(t *testing.T) {
	redisServer, _ := useRateLimitMiniRedis(t)

	allowed, _, err := takeManagedTokenLimit(context.Background(), "test:redis:tpm", 100, 60)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, retryAfter, err := takeManagedTokenLimit(context.Background(), "test:redis:tpm", 100, 41)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.EqualValues(t, 60, retryAfter)
	stored, err := redisServer.Get("test:redis:tpm")
	require.NoError(t, err)
	assert.Equal(t, "60", stored)

	allowed, _, err = takeManagedTokenLimit(context.Background(), "test:redis:other-token", 100, 100)
	require.NoError(t, err)
	assert.True(t, allowed, "different managed keys must not share a TPM counter")
}

func TestEnforceManagedTokenRPMReturnsOpenAI429(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	resetManagedTokenWindowsForTest()
	t.Cleanup(func() {
		common.RedisEnabled = previousRedisEnabled
		resetManagedTokenWindowsForTest()
	})

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(ctx, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)
		common.SetContextKey(ctx, constant.ContextKeyTokenId, 101)
		common.SetContextKey(ctx, constant.ContextKeyTokenRPM, 1)
		return ctx, recorder
	}

	first, _ := newContext()
	assert.True(t, EnforceManagedTokenRPM(first))
	second, secondRecorder := newContext()
	assert.False(t, EnforceManagedTokenRPM(second))
	assert.Equal(t, http.StatusTooManyRequests, secondRecorder.Code)
	assert.NotEmpty(t, secondRecorder.Header().Get("Retry-After"))
	assert.Contains(t, secondRecorder.Body.String(), `"code":"rate_limit_exceeded"`)

	ordinaryRecorder := httptest.NewRecorder()
	ordinary, _ := gin.CreateTestContext(ordinaryRecorder)
	ordinary.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	assert.True(t, EnforceManagedTokenRPM(ordinary))
	assert.Equal(t, http.StatusOK, ordinaryRecorder.Code)
}

func TestReserveManagedTokenTPMUsesPerKeyBudget(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	resetManagedTokenWindowsForTest()
	t.Cleanup(func() {
		common.RedisEnabled = previousRedisEnabled
		resetManagedTokenWindowsForTest()
	})

	newContext := func(tokenID int) *gin.Context {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(ctx, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)
		common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
		common.SetContextKey(ctx, constant.ContextKeyTokenTPM, 100)
		return ctx
	}

	allowed, _, err := ReserveManagedTokenTPM(newContext(201), 40, 20)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, retryAfter, err := ReserveManagedTokenTPM(newContext(201), 21, 20)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Positive(t, retryAfter)
	allowed, _, err = ReserveManagedTokenTPM(newContext(202), 80, 20)
	require.NoError(t, err)
	assert.True(t, allowed, "different managed keys must not share a TPM counter")
}
