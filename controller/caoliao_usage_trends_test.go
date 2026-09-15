package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useCaoliaoUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	databaseName := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open("file:"+databaseName+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB })
	return db
}

func TestCaoliaoMockUsageFeedsTrendInterface(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	t.Setenv("CAOLIAO_ENABLE_MOCK_USAGE", "true")
	user := model.User{Username: "mock-user", Password: "unused", DisplayName: "Mock", Status: 1, Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "mock-key", Name: "Mock Key", Status: 1, ManagedBy: model.CaoliaoManagedBy, ExpiredTime: -1}
	require.NoError(t, db.Create(&token).Error)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/usage/mock", PostCaoliaoMockUsage)
	router.GET("/usage", GetCaoliaoUsage)
	body := `{"dataset":"local-demo","replace":true,"items":[` +
		`{"token_id":` + strconv.Itoa(token.Id) + `,"model_id":"deepseek-v4-flash","occurred_at":"2026-08-20T12:00:00+08:00","successes":3,"failures":1,"input_tokens":900,"output_tokens":300},` +
		`{"token_id":` + strconv.Itoa(token.Id) + `,"model_id":"qwen3.6","occurred_at":"2026-08-20T12:00:00+08:00","successes":2,"failures":0,"input_tokens":400,"output_tokens":100}]}`

	seed := httptest.NewRequest(http.MethodPost, "/usage/mock", strings.NewReader(body))
	seed.Header.Set("Content-Type", "application/json")
	seedResponse := httptest.NewRecorder()
	router.ServeHTTP(seedResponse, seed)
	require.Equal(t, http.StatusOK, seedResponse.Code, seedResponse.Body.String())

	trend := httptest.NewRequest(http.MethodGet, "/usage?view=trend&models=deepseek-v4-flash,qwen3.6&start=2026-08-20&end=2026-08-20", nil)
	trendResponse := httptest.NewRecorder()
	router.ServeHTTP(trendResponse, trend)
	require.Equal(t, http.StatusOK, trendResponse.Code, trendResponse.Body.String())
	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal(trendResponse.Body.Bytes(), &decoded))
	data := decoded["data"].(map[string]any)
	summary := data["summary"].(map[string]any)
	assert.EqualValues(t, 6, summary["requests"])
	assert.EqualValues(t, 1300, summary["input_tokens"])
	assert.Len(t, data["models"], 2)
}

func TestCaoliaoMockUsageIsDisabledByDefault(t *testing.T) {
	useCaoliaoUsageTestDB(t)
	t.Setenv("CAOLIAO_ENABLE_MOCK_USAGE", "false")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/usage/mock", PostCaoliaoMockUsage)
	request := httptest.NewRequest(http.MethodPost, "/usage/mock", strings.NewReader(`{"dataset":"demo","items":[]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func TestCaoliaoUsageDateRangeUsesShanghaiBoundaries(t *testing.T) {
	start, err := parseCaoliaoUsageTime("2026-08-20", false)
	require.NoError(t, err)
	end, err := parseCaoliaoUsageTime("2026-08-20", true)
	require.NoError(t, err)
	expectedStart := time.Date(2026, 8, 20, 0, 0, 0, 0, caoliaoUsageLocation).Unix()
	assert.Equal(t, expectedStart, start)
	assert.Equal(t, expectedStart+24*60*60-1, end)
}

func TestCaoliaoMockUsageRejectsTokenOverflow(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	t.Setenv("CAOLIAO_ENABLE_MOCK_USAGE", "true")
	user := model.User{Username: "overflow-user", Password: "unused", DisplayName: "Overflow", Status: 1, Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "overflow-key", Name: "Overflow Key", Status: 1, ManagedBy: model.CaoliaoManagedBy, ExpiredTime: -1}
	require.NoError(t, db.Create(&token).Error)
	body := `{"dataset":"overflow","items":[{"token_id":` + strconv.Itoa(token.Id) +
		`,"model_id":"qwen3.6","occurred_at":"2026-08-20T12:00:00+08:00","successes":1,"failures":0,"input_tokens":` +
		strconv.Itoa(common.MaxQuota) + `,"output_tokens":1}]}`
	request := httptest.NewRequest(http.MethodPost, "/usage/mock", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router := gin.New()
	router.POST("/usage/mock", PostCaoliaoMockUsage)
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "invalid usage item")
}

// seedCaoliaoTrendUsage creates two managed tokens and seeds both models for each,
// so the per-token breakdown must produce 2 tokens x 2 models rows.
func seedCaoliaoTrendUsage(t *testing.T, db *gorm.DB) []int {
	t.Helper()
	t.Setenv("CAOLIAO_ENABLE_MOCK_USAGE", "true")
	user := model.User{Username: "breakdown-user", Password: "unused", DisplayName: "Breakdown", Status: 1, Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	ids := make([]int, 0, 2)
	items := make([]string, 0, 4)
	for index := 1; index <= 2; index++ {
		token := model.Token{UserId: user.Id, Key: "breakdown-key-" + strconv.Itoa(index),
			Name: "Breakdown Key " + strconv.Itoa(index), Status: 1,
			ManagedBy: model.CaoliaoManagedBy, ExpiredTime: -1}
		require.NoError(t, db.Create(&token).Error)
		ids = append(ids, token.Id)
		id := strconv.Itoa(token.Id)
		items = append(items,
			`{"token_id":`+id+`,"model_id":"deepseek-v4-flash","occurred_at":"2026-08-20T12:00:00+08:00","successes":3,"failures":1,"input_tokens":900,"output_tokens":300}`,
			`{"token_id":`+id+`,"model_id":"qwen3.6","occurred_at":"2026-08-20T12:00:00+08:00","successes":2,"failures":0,"input_tokens":400,"output_tokens":100}`)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/usage/mock", PostCaoliaoMockUsage)
	router.GET("/usage", GetCaoliaoUsage)
	seed := httptest.NewRequest(http.MethodPost, "/usage/mock", strings.NewReader(
		`{"dataset":"breakdown-demo","replace":true,"items":[`+strings.Join(items, ",")+`]}`))
	seed.Header.Set("Content-Type", "application/json")
	seedResponse := httptest.NewRecorder()
	router.ServeHTTP(seedResponse, seed)
	require.Equal(t, http.StatusOK, seedResponse.Code, seedResponse.Body.String())
	return ids
}

func caoliaoTrendRequest(t *testing.T, db *gorm.DB, tokenIDs []int, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	router := gin.New()
	router.GET("/usage", GetCaoliaoUsage)
	request := httptest.NewRequest(http.MethodGet, "/usage?view=trend&models=deepseek-v4-flash,qwen3.6"+
		"&start=2026-08-20&end=2026-08-20&token_ids="+joinInts(tokenIDs)+"&"+query, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return response, nil
	}
	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	return response, decoded["data"].(map[string]any)
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func TestCaoliaoTrendUsageSplitsEveryTokenByModelWhenIncluded(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	ids := seedCaoliaoTrendUsage(t, db)
	response, data := caoliaoTrendRequest(t, db, ids, "include=tokens")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	rows, ok := data["tokens"].([]any)
	require.True(t, ok, "include=tokens 必须返回分 Key 分模型明细")
	require.Len(t, rows, 4, "2 枚 Key × 2 个模型")

	seen := map[string]bool{}
	// 逐行求和必须回到 summary：这条断言防的是漏行与重复计数。
	totals := map[string]float64{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		modelID := row["model_id"].(string)
		tokenID := int(row["token_id"].(float64))
		assert.Contains(t, ids, tokenID)
		assert.Contains(t, []string{"deepseek-v4-flash", "qwen3.6"}, modelID)
		seen[strconv.Itoa(tokenID)+"/"+modelID] = true
		for _, field := range []string{"requests", "input_tokens", "output_tokens", "charged_tokens", "successes", "failures"} {
			totals[field] += row[field].(float64)
		}
	}
	assert.Len(t, seen, 4, "每枚 Key 的每个模型最多一行")

	summary := data["summary"].(map[string]any)
	for field, total := range totals {
		assert.EqualValues(t, summary[field], total, "分 Key 明细的 "+field+" 合计必须等于 summary")
	}
}

func TestCaoliaoTrendUsageOmitsTokenBreakdownByDefault(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	ids := seedCaoliaoTrendUsage(t, db)
	response, data := caoliaoTrendRequest(t, db, ids, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	// 员工个人中心的 /api/me/usage 也走这个视图。它不传 include，
	// 所以响应必须与改动前逐字节一致 —— 键不存在，而不是空数组。
	_, present := data["tokens"]
	assert.False(t, present, "未请求时不得出现 tokens 键")
	assert.NotContains(t, response.Body.String(), `"tokens"`)
	assert.Len(t, data["models"], 2, "图表数据不受影响")
}

func TestCaoliaoTrendUsageRejectsUnknownIncludeValues(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	ids := seedCaoliaoTrendUsage(t, db)
	for _, value := range []string{"items", "1", "tokens,items"} {
		response, _ := caoliaoTrendRequest(t, db, ids, "include="+value)
		assert.Equal(t, http.StatusBadRequest, response.Code, value)
		assert.Contains(t, response.Body.String(), "invalid include", value)
	}
}

func TestCaoliaoTrendUsageBreakdownCarriesNoContentOrSecrets(t *testing.T) {
	db := useCaoliaoUsageTestDB(t)
	ids := seedCaoliaoTrendUsage(t, db)
	response, data := caoliaoTrendRequest(t, db, ids, "include=tokens")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	body := response.Body.String()
	for _, forbidden := range []string{"prompt", "content", "secret", "api_key", "token_name"} {
		assert.NotContains(t, body, forbidden)
	}
	allowed := map[string]bool{"token_id": true, "model_id": true, "requests": true,
		"input_tokens": true, "output_tokens": true, "charged_tokens": true,
		"successes": true, "failures": true, "latency_ms": true, "rate_limit_errors": true}
	for _, raw := range data["tokens"].([]any) {
		for key := range raw.(map[string]any) {
			assert.True(t, allowed[key], "分 Key 明细只允许暴露元数据字段，多出："+key)
		}
	}
}

func TestCaoliaoTrendUsageRejectsInvalidOrUnmanagedTokenFilters(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		seed       func(*testing.T, *gorm.DB) int
		wantStatus int
		wantBody   string
	}{
		{name: "invalid token list", query: "token_ids=abc", wantStatus: http.StatusBadRequest, wantBody: "invalid token_ids"},
		{name: "unknown token", query: "token_id=999999", wantStatus: http.StatusNotFound, wantBody: "key not found"},
		{name: "missing tokens in aggregate filter", query: "token_ids=999999", wantStatus: http.StatusOK, wantBody: `"requests":0`},
		{name: "unmanaged token", wantStatus: http.StatusNotFound, wantBody: "key not found", seed: func(t *testing.T, db *gorm.DB) int {
			token := model.Token{UserId: 1, Key: "unmanaged", Name: "Unmanaged", Status: 1, ManagedBy: "", ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			return token.Id
		}},
		{name: "soft deleted token", wantStatus: http.StatusNotFound, wantBody: "key not found", seed: func(t *testing.T, db *gorm.DB) int {
			token := model.Token{UserId: 1, Key: "deleted", Name: "Deleted", Status: 1, ManagedBy: model.CaoliaoManagedBy, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			require.NoError(t, db.Delete(&token).Error)
			return token.Id
		}},
		{name: "invalid model", query: "models=bad%20model", wantStatus: http.StatusBadRequest, wantBody: "invalid models", seed: func(t *testing.T, db *gorm.DB) int {
			token := model.Token{UserId: 1, Key: "managed", Name: "Managed", Status: 1, ManagedBy: model.CaoliaoManagedBy, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			return token.Id
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := useCaoliaoUsageTestDB(t)
			query := test.query
			if test.seed != nil {
				tokenID := test.seed(t, db)
				if !strings.Contains(query, "token_ids=") && !strings.Contains(query, "token_id=") {
					if query != "" {
						query += "&"
					}
					query += "token_id=" + strconv.Itoa(tokenID)
				}
			}
			request := httptest.NewRequest(http.MethodGet,
				"/usage?view=trend&start=2026-08-20&end=2026-08-20&"+query, nil)
			response := httptest.NewRecorder()
			router := gin.New()
			router.GET("/usage", GetCaoliaoUsage)
			router.ServeHTTP(response, request)
			assert.Equal(t, test.wantStatus, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), test.wantBody)
		})
	}
}
