package model

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTokenCacheRoundTripsCaoliaoManagedLimits(t *testing.T) {
	useUserCacheMiniRedis(t)
	token := Token{
		Id:                  901,
		UserId:              902,
		Key:                 "caoliao-cache-roundtrip",
		Name:                "managed cache",
		Status:              common.TokenStatusEnabled,
		ExpiredTime:         -1,
		UnlimitedQuota:      true,
		ManagedBy:           CaoliaoManagedBy,
		RequestsPerTwoHours: 37,
		TokensPerTwoHours:   12_345,
		DailyTokenQuota:     67_890,
		ManagedLimitVersion: 1,
	}

	result, err := cacheInitToken(token)
	require.NoError(t, err)
	assert.Equal(t, 1, result)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, CaoliaoManagedBy, cached.ManagedBy)
	assert.Equal(t, 37, cached.RequestsPerTwoHours)
	assert.Equal(t, 12_345, cached.TokensPerTwoHours)
	assert.Equal(t, 67_890, cached.DailyTokenQuota)
	assert.Equal(t, 1, cached.ManagedLimitVersion)
	assert.True(t, cached.UnlimitedQuota)

	require.NoError(t, common.RDB.HDel(context.Background(), getTokenCacheKey(token.Key),
		"ManagedLimitVersion").Err())
	_, err = cacheGetTokenByKey(token.Key)
	assert.ErrorContains(t, err, "legacy limit schema")
}

func TestMigrateCaoliaoManagedTokenLimitsClearsMinutesAndKeepsLifetimeQuota(t *testing.T) {
	previousDB := DB
	db, err := gorm.Open(sqlite.Open("file:caoliao-limit-migration-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Token{}))
	require.NoError(t, db.Exec("ALTER TABLE tokens ADD COLUMN requests_per_minute integer DEFAULT 0").Error)
	require.NoError(t, db.Exec("ALTER TABLE tokens ADD COLUMN tokens_per_minute integer DEFAULT 0").Error)
	DB = db
	t.Cleanup(func() { DB = previousDB })

	legacy := Token{UserId: 1, Key: "legacy-limits", Name: "legacy", ManagedBy: CaoliaoManagedBy, RemainQuota: 123, UsedQuota: 77}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, db.Table("tokens").Where("id = ?", legacy.Id).Updates(map[string]interface{}{
		"requests_per_minute": 20,
		"tokens_per_minute":   5_000,
	}).Error)
	unlimited := Token{UserId: 2, Key: "new-unlimited", Name: "new", ManagedBy: CaoliaoManagedBy, ManagedLimitVersion: 1}
	require.NoError(t, db.Create(&unlimited).Error)

	require.NoError(t, migrateCaoliaoManagedTokenLimits())
	var migrated Token
	require.NoError(t, db.First(&migrated, legacy.Id).Error)
	assert.Equal(t, DefaultCaoliaoRequestsPerTwoHours, migrated.RequestsPerTwoHours)
	assert.Equal(t, DefaultCaoliaoOutputTokensPerTwoHours, migrated.TokensPerTwoHours)
	assert.Equal(t, DefaultCaoliaoDailyOutputTokenQuota, migrated.DailyTokenQuota)
	assert.Equal(t, 1, migrated.ManagedLimitVersion)
	assert.Equal(t, 123, migrated.RemainQuota)
	assert.Equal(t, 77, migrated.UsedQuota)
	var cleared struct {
		RequestsPerMinute int `gorm:"column:requests_per_minute"`
		TokensPerMinute   int `gorm:"column:tokens_per_minute"`
	}
	require.NoError(t, db.Table("tokens").Select("requests_per_minute", "tokens_per_minute").Where("id = ?", legacy.Id).Scan(&cleared).Error)
	assert.Zero(t, cleared.RequestsPerMinute)
	assert.Zero(t, cleared.TokensPerMinute)

	require.NoError(t, migrateCaoliaoManagedTokenLimits())
	var preserved Token
	require.NoError(t, db.First(&preserved, unlimited.Id).Error)
	assert.Zero(t, preserved.RequestsPerTwoHours)
	assert.Zero(t, preserved.TokensPerTwoHours)
	assert.Zero(t, preserved.DailyTokenQuota)
}

func TestQueryCaoliaoDailyUsageGroupsByShanghaiDayAndModel(t *testing.T) {
	previousDB, previousLogDB := DB, LOG_DB
	db, err := gorm.Open(sqlite.Open("file:caoliao-daily-main-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	logDB, err := gorm.Open(sqlite.Open("file:caoliao-daily-log-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&Log{}))
	DB, LOG_DB = db, logDB
	t.Cleanup(func() { DB, LOG_DB = previousDB, previousLogDB })

	lateUTC := time.Date(2026, 8, 1, 16, 30, 0, 0, time.UTC).Unix()
	logs := []Log{
		{TokenId: 7, CreatedAt: lateUTC, Type: LogTypeConsume, ModelName: "deepseek-v4-flash", PromptTokens: 100, CompletionTokens: 20, Quota: 120},
		{TokenId: 8, CreatedAt: lateUTC + 10, Type: LogTypeError, ModelName: "deepseek-v4-flash", Content: "caoliao_rate_limit:tokens_daily"},
		{TokenId: 7, CreatedAt: lateUTC + 20, Type: LogTypeConsume, ModelName: "qwen3.6", PromptTokens: 50, CompletionTokens: 10, Quota: 60},
		{TokenId: 9, CreatedAt: lateUTC + 30, Type: LogTypeConsume, ModelName: "deepseek-v4-flash", PromptTokens: 999, CompletionTokens: 999, Quota: 1998},
	}
	require.NoError(t, logDB.Create(&logs).Error)

	usage, err := QueryCaoliaoDailyUsage([]int{7, 8}, []string{"deepseek-v4-flash", "qwen3.6"}, lateUTC-60, lateUTC+60)
	require.NoError(t, err)
	require.Len(t, usage, 2)
	assert.Equal(t, "2026-08-02", usage[0].UsageDay)
	assert.Equal(t, "deepseek-v4-flash", usage[0].ModelName)
	assert.EqualValues(t, 1, usage[0].SuccessfulRequests)
	assert.EqualValues(t, 1, usage[0].FailedRequests)
	assert.EqualValues(t, 100, usage[0].InputTokens)
	assert.EqualValues(t, 20, usage[0].OutputTokens)
	assert.EqualValues(t, 1, usage[0].RateLimitErrors)
	assert.Equal(t, "qwen3.6", usage[1].ModelName)
}

func TestCaoliaoUsageDayExpressionSupportsEveryLogDatabase(t *testing.T) {
	tests := []struct {
		dialect  string
		contains string
	}{
		{dialect: "sqlite", contains: "strftime"},
		{dialect: "postgres", contains: "AT TIME ZONE 'Asia/Shanghai'"},
		{dialect: "mysql", contains: "1970-01-01 08:00:00"},
		{dialect: "clickhouse", contains: "formatDateTime"},
	}
	for _, test := range tests {
		t.Run(test.dialect, func(t *testing.T) {
			expression, err := caoliaoUsageDayExpression(test.dialect)
			require.NoError(t, err)
			assert.Contains(t, expression, test.contains)
		})
	}
	_, err := caoliaoUsageDayExpression("unknown")
	assert.ErrorContains(t, err, "unsupported log database dialect")
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
		{TokenId: 7, CreatedAt: 102, Type: LogTypeError, Content: "caoliao_rate_limit:requests_2h"},
		{TokenId: 7, CreatedAt: 103, Type: LogTypeError, Content: "caoliao_rate_limit:rpm"},
		{TokenId: 8, CreatedAt: 104, Type: LogTypeConsume, PromptTokens: 99, CompletionTokens: 99, Quota: 198, Content: "different key"},
	}
	require.NoError(t, db.Create(&logs).Error)

	usage, err := QueryCaoliaoUsage([]int{7}, 100, 103)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, 7, usage[0].TokenId)
	assert.EqualValues(t, 1, usage[0].SuccessfulRequests)
	assert.EqualValues(t, 3, usage[0].FailedRequests)
	assert.EqualValues(t, 10, usage[0].InputTokens)
	assert.EqualValues(t, 5, usage[0].OutputTokens)
	assert.EqualValues(t, 15, usage[0].ChargedTokens)
	assert.InDelta(t, 0.75, usage[0].AverageLatencySecond, 0.001)
	assert.EqualValues(t, 2, usage[0].RateLimitErrors)
}
