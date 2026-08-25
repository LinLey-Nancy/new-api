package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTokenCacheRoundTripsCaoliaoManagedLimits(t *testing.T) {
	useUserCacheMiniRedis(t)
	token := Token{
		Id:                901,
		UserId:            902,
		Key:               "caoliao-cache-roundtrip",
		Name:              "managed cache",
		Status:            common.TokenStatusEnabled,
		ExpiredTime:       -1,
		UnlimitedQuota:    true,
		ManagedBy:         CaoliaoManagedBy,
		RequestsPerMinute: 37,
		TokensPerMinute:   12_345,
	}

	result, err := cacheInitToken(token)
	require.NoError(t, err)
	assert.Equal(t, 1, result)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, CaoliaoManagedBy, cached.ManagedBy)
	assert.Equal(t, 37, cached.RequestsPerMinute)
	assert.Equal(t, 12_345, cached.TokensPerMinute)
	assert.True(t, cached.UnlimitedQuota)
}

func TestQueryCaoliaoUsageAggregatesMetadataOnly(t *testing.T) {
	previousDB, previousLogDB := DB, LOG_DB
	db, err := gorm.Open(sqlite.Open("file:caoliao-usage-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))
	DB, LOG_DB = db, db
	t.Cleanup(func() { DB, LOG_DB = previousDB, previousLogDB })

	logs := []Log{
		{TokenId: 7, CreatedAt: 100, Type: LogTypeConsume, PromptTokens: 10, CompletionTokens: 5, Quota: 15, UseTime: 2, Content: "billing metadata"},
		{TokenId: 7, CreatedAt: 101, Type: LogTypeError, UseTime: 1, Content: "upstream error without request body"},
		{TokenId: 7, CreatedAt: 102, Type: LogTypeError, Content: "caoliao_rate_limit:rpm"},
		{TokenId: 8, CreatedAt: 103, Type: LogTypeConsume, PromptTokens: 99, CompletionTokens: 99, Quota: 198, Content: "different key"},
	}
	require.NoError(t, db.Create(&logs).Error)

	usage, err := QueryCaoliaoUsage([]int{7}, 100, 102)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, 7, usage[0].TokenId)
	assert.EqualValues(t, 1, usage[0].SuccessfulRequests)
	assert.EqualValues(t, 2, usage[0].FailedRequests)
	assert.EqualValues(t, 10, usage[0].InputTokens)
	assert.EqualValues(t, 5, usage[0].OutputTokens)
	assert.EqualValues(t, 15, usage[0].ChargedTokens)
	assert.InDelta(t, 1, usage[0].AverageLatencySecond, 0.001)
	assert.EqualValues(t, 1, usage[0].RateLimitErrors)
}
