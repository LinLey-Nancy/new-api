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
