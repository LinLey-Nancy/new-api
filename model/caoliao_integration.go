package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	CaoliaoManagedBy      = "caoliao"
	caoliaoEmployeePrefix = "caoliao:employee:"
)

type CaoliaoUsage struct {
	TokenId              int     `json:"token_id" gorm:"column:token_id"`
	SuccessfulRequests   int64   `json:"successful_requests" gorm:"column:successful_requests"`
	FailedRequests       int64   `json:"failed_requests" gorm:"column:failed_requests"`
	InputTokens          int64   `json:"input_tokens" gorm:"column:input_tokens"`
	OutputTokens         int64   `json:"output_tokens" gorm:"column:output_tokens"`
	ChargedTokens        int64   `json:"charged_tokens" gorm:"column:charged_tokens"`
	AverageLatencySecond float64 `json:"-" gorm:"column:average_latency_seconds"`
	RateLimitErrors      int64   `json:"rate_limit_errors" gorm:"column:rate_limit_errors"`
}

type CaoliaoDailyUsage struct {
	ModelName          string `json:"model_id" gorm:"column:model_name"`
	UsageDay           string `json:"date" gorm:"column:usage_day"`
	SuccessfulRequests int64  `json:"successes" gorm:"column:successful_requests"`
	FailedRequests     int64  `json:"failures" gorm:"column:failed_requests"`
	InputTokens        int64  `json:"input_tokens" gorm:"column:input_tokens"`
	OutputTokens       int64  `json:"output_tokens" gorm:"column:output_tokens"`
	ChargedTokens      int64  `json:"charged_tokens" gorm:"column:charged_tokens"`
	RateLimitErrors    int64  `json:"rate_limit_errors" gorm:"column:rate_limit_errors"`
}

// EnsureCaoliaoEmployee returns the non-interactive New API user associated
// with an employee. The deterministic username makes repeated/concurrent calls
// idempotent without exposing the New API user ID as a business identifier.
func EnsureCaoliaoEmployee(employeeID string, displayName string) (*User, error) {
	marker := caoliaoEmployeePrefix + employeeID
	user, err := GetCaoliaoEmployee(employeeID)
	if err == nil {
		return updateCaoliaoEmployeeDisplayName(user, displayName)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	hash := sha256.Sum256([]byte(employeeID))
	password, err := common.GenerateKey()
	if err != nil {
		return nil, err
	}
	password, err = common.Password2Hash(password)
	if err != nil {
		return nil, err
	}

	user = &User{
		Username:    "cl_" + hex.EncodeToString(hash[:])[:16],
		Password:    password,
		DisplayName: truncateCaoliaoDisplayName(displayName),
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       common.MaxQuota,
		Group:       "default",
		AffCode:     common.GetRandomString(12),
		Setting:     "{}",
		Remark:      marker,
		AuthVersion: 1,
	}
	if err = DB.Create(user).Error; err == nil {
		return user, nil
	}

	// A concurrent idempotent request can lose the unique username race. Return
	// the winner when it owns the same employee marker.
	concurrentUser, lookupErr := GetCaoliaoEmployee(employeeID)
	if lookupErr == nil {
		return updateCaoliaoEmployeeDisplayName(concurrentUser, displayName)
	}
	return nil, err
}

func GetCaoliaoEmployee(employeeID string) (*User, error) {
	user := &User{}
	err := DB.Where("remark = ?", caoliaoEmployeePrefix+employeeID).First(user).Error
	return user, err
}

func updateCaoliaoEmployeeDisplayName(user *User, displayName string) (*User, error) {
	normalizedName := truncateCaoliaoDisplayName(displayName)
	if user.DisplayName == normalizedName {
		return user, nil
	}
	if err := DB.Model(user).Update("display_name", normalizedName).Error; err != nil {
		return nil, err
	}
	user.DisplayName = normalizedName
	if err := updateUserCache(*user); err != nil {
		common.SysLog("failed to refresh caoliao employee cache: " + err.Error())
	}
	return user, nil
}

func truncateCaoliaoDisplayName(displayName string) string {
	runes := []rune(strings.TrimSpace(displayName))
	if len(runes) > UserNameMaxLength {
		runes = runes[:UserNameMaxLength]
	}
	return string(runes)
}

func ListCaoliaoTokensByEmployee(employeeID string) ([]*Token, error) {
	user, err := GetCaoliaoEmployee(employeeID)
	if err != nil {
		return nil, err
	}
	return ListCaoliaoTokensByUserID(user.Id)
}

func ListCaoliaoTokensByUserID(userID int) ([]*Token, error) {
	var tokens []*Token
	err := DB.Where("user_id = ? AND managed_by = ?", userID, CaoliaoManagedBy).
		Order("id DESC").Find(&tokens).Error
	return tokens, err
}

func ListAllCaoliaoTokens() ([]*Token, error) {
	var tokens []*Token
	err := DB.Where("managed_by = ?", CaoliaoManagedBy).Order("id DESC").Find(&tokens).Error
	return tokens, err
}

func GetCaoliaoManagedToken(id int) (*Token, error) {
	token := &Token{}
	err := DB.Where("id = ? AND managed_by = ?", id, CaoliaoManagedBy).First(token).Error
	return token, err
}

func CreateCaoliaoToken(userID int, name string, quota int, requestsPerMinute int, tokensPerMinute int, expiresAt int64) (*Token, string, error) {
	rawKey, err := common.GenerateKey()
	if err != nil {
		return nil, "", err
	}
	unlimitedQuota := quota == -1
	remainQuota := quota
	if unlimitedQuota {
		remainQuota = 0
	}
	token := &Token{
		UserId:            userID,
		Key:               rawKey,
		Status:            common.TokenStatusEnabled,
		Name:              name,
		CreatedTime:       common.GetTimestamp(),
		AccessedTime:      common.GetTimestamp(),
		ExpiredTime:       expiresAt,
		RemainQuota:       remainQuota,
		UnlimitedQuota:    unlimitedQuota,
		Group:             "default",
		ManagedBy:         CaoliaoManagedBy,
		RequestsPerMinute: requestsPerMinute,
		TokensPerMinute:   tokensPerMinute,
	}
	if err = token.Insert(); err != nil {
		return nil, "", err
	}
	return token, "sk-" + rawKey, nil
}

func UpdateCaoliaoManagedToken(token *Token) error {
	if token == nil || token.ManagedBy != CaoliaoManagedBy {
		return gorm.ErrRecordNotFound
	}
	return token.Update()
}

func DeleteCaoliaoManagedToken(token *Token) error {
	if token == nil || token.ManagedBy != CaoliaoManagedBy {
		return gorm.ErrRecordNotFound
	}
	return token.Delete()
}

// QueryCaoliaoUsage aggregates only billing and error rows. Prompt and response
// bodies are deliberately never selected or returned.
func QueryCaoliaoUsage(tokenIDs []int, startAt int64, endAt int64) ([]CaoliaoUsage, error) {
	if len(tokenIDs) == 0 {
		return []CaoliaoUsage{}, nil
	}
	var rows []CaoliaoUsage
	err := LOG_DB.Model(&Log{}).
		Select(`token_id,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS successful_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS failed_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) AS input_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS output_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS charged_tokens,
			COALESCE(AVG(CASE WHEN type IN (?, ?) THEN use_time ELSE NULL END), 0) AS average_latency_seconds,
			COALESCE(SUM(CASE WHEN type = ? AND content IN (?, ?) THEN 1 ELSE 0 END), 0) AS rate_limit_errors`,
			LogTypeConsume,
			LogTypeError,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume, LogTypeError,
			LogTypeError, "caoliao_rate_limit:rpm", "caoliao_rate_limit:tpm").
		Where("token_id IN ?", tokenIDs).
		Where("created_at >= ? AND created_at <= ?", startAt, endAt).
		Where("type IN ?", []int{LogTypeConsume, LogTypeError}).
		Group("token_id").
		Order("token_id ASC").
		Scan(&rows).Error
	return rows, err
}

// QueryCaoliaoDailyUsage returns metadata-only daily aggregates in the
// deployment's reporting timezone (Asia/Shanghai). It deliberately excludes
// prompts and responses and only accepts already-authorized managed token IDs.
func QueryCaoliaoDailyUsage(tokenIDs []int, modelNames []string, startAt int64, endAt int64) ([]CaoliaoDailyUsage, error) {
	if len(tokenIDs) == 0 || len(modelNames) == 0 {
		return []CaoliaoDailyUsage{}, nil
	}
	dayExpression, err := caoliaoUsageDayExpression(LOG_DB.Dialector.Name())
	if err != nil {
		return nil, err
	}
	var rows []CaoliaoDailyUsage
	err = LOG_DB.Model(&Log{}).
		Select(fmt.Sprintf(`model_name, %s AS usage_day,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS successful_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS failed_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) AS input_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS output_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS charged_tokens,
			COALESCE(SUM(CASE WHEN type = ? AND content IN (?, ?) THEN 1 ELSE 0 END), 0) AS rate_limit_errors`, dayExpression),
			LogTypeConsume,
			LogTypeError,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeError, "caoliao_rate_limit:rpm", "caoliao_rate_limit:tpm").
		Where("token_id IN ?", tokenIDs).
		Where("model_name IN ?", modelNames).
		Where("created_at >= ? AND created_at <= ?", startAt, endAt).
		Where("type IN ?", []int{LogTypeConsume, LogTypeError}).
		Group("model_name, usage_day").
		Order("usage_day ASC, model_name ASC").
		Scan(&rows).Error
	return rows, err
}

func caoliaoUsageDayExpression(dialect string) (string, error) {
	switch dialect {
	case "sqlite":
		return "strftime('%Y-%m-%d', created_at, 'unixepoch', '+8 hours')", nil
	case "postgres":
		return "TO_CHAR(TO_TIMESTAMP(created_at) AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD')", nil
	case "mysql":
		return "DATE_FORMAT(DATE_ADD('1970-01-01 08:00:00', INTERVAL created_at SECOND), '%Y-%m-%d')", nil
	case "clickhouse":
		return "formatDateTime(toDateTime(created_at, 'UTC'), '%Y-%m-%d', 'Asia/Shanghai')", nil
	default:
		return "", fmt.Errorf("unsupported log database dialect for caoliao usage: %s", dialect)
	}
}

// ReplaceCaoliaoMockUsage replaces one explicitly named development dataset.
// The caller must enforce the environment gate and managed-token ownership.
func ReplaceCaoliaoMockUsage(dataset string, logs []Log, replace bool) (int64, int64, error) {
	marker := "caoliao_mock_usage:" + dataset
	var deleted int64
	err := LOG_DB.Transaction(func(tx *gorm.DB) error {
		if replace {
			result := tx.Where("content = ?", marker).Delete(&Log{})
			if result.Error != nil {
				return result.Error
			}
			deleted = result.RowsAffected
		}
		if len(logs) == 0 {
			return nil
		}
		return tx.CreateInBatches(&logs, 500).Error
	})
	return deleted, int64(len(logs)), err
}

func GetCaoliaoIntegrationHealth(ctx context.Context) (int64, error) {
	if DB == nil || LOG_DB == nil {
		return 0, errors.New("database is not initialized")
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return 0, err
	}
	if err = sqlDB.PingContext(ctx); err != nil {
		return 0, err
	}
	if LOG_DB != DB {
		logDB, dbErr := LOG_DB.DB()
		if dbErr != nil {
			return 0, dbErr
		}
		if err = logDB.PingContext(ctx); err != nil {
			return 0, err
		}
	}
	var count int64
	err = DB.WithContext(ctx).Model(&Token{}).Where("managed_by = ?", CaoliaoManagedBy).Count(&count).Error
	return count, err
}

// RecordCaoliaoRateLimitLog records a metadata-only error event for usage
// aggregation. It never reads or stores request prompts or response bodies.
func RecordCaoliaoRateLimitLog(c *gin.Context, kind string) {
	if kind != "rpm" && kind != "tpm" {
		return
	}
	if common.GetContextKeyString(c, constant.ContextKeyTokenManagedBy) != CaoliaoManagedBy {
		return
	}
	userID := c.GetInt("id")
	tokenID := c.GetInt("token_id")
	if userID <= 0 || tokenID <= 0 {
		return
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group == "" {
		group = common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	}
	modelName := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	RecordErrorLog(
		c,
		userID,
		0,
		modelName,
		c.GetString("token_name"),
		fmt.Sprintf("caoliao_rate_limit:%s", kind),
		tokenID,
		0,
		false,
		group,
		map[string]interface{}{"caoliao_rate_limit": kind},
	)
}
