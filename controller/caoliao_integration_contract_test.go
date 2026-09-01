package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaykittypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCaoliaoKeySecretIsReturnedOnlyOnCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open("file:caoliao-secret-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	t.Cleanup(func() { model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLogDB, previousRedisEnabled })

	employee, err := model.EnsureCaoliaoEmployee("E-001", "????")
	require.NoError(t, err)
	require.NotZero(t, employee.Id)

	createBody := `{"name":"?? Key","token_quota":10000,"requests_per_two_hours":2400,"tokens_per_two_hours":600000,"daily_token_quota":7200000,"expires_at":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`
	createResponse := caoliaoContractRequest(http.MethodPost, "/employees/E-001/keys", createBody)
	assert.Equal(t, http.StatusOK, createResponse.Code)
	assert.Contains(t, createResponse.Body.String(), `"secret":"sk-`)
	assert.Contains(t, createResponse.Body.String(), `"key_mask":"sk-`)
	assert.Contains(t, createResponse.Body.String(), `"requests_per_two_hours":2400`)
	assert.Contains(t, createResponse.Body.String(), `"tokens_per_two_hours":600000`)
	assert.Contains(t, createResponse.Body.String(), `"daily_token_quota":7200000`)
	assert.NotContains(t, createResponse.Body.String(), `"requests_per_minute"`)
	assert.NotContains(t, createResponse.Body.String(), `"tokens_per_minute"`)

	listResponse := caoliaoContractRequest(http.MethodGet, "/employees/E-001/keys", "")
	assert.Equal(t, http.StatusOK, listResponse.Code)
	assert.NotContains(t, listResponse.Body.String(), `"secret"`)
	assert.NotContains(t, listResponse.Body.String(), `"key":`)
	assert.Contains(t, listResponse.Body.String(), `"key_mask":"sk-`)

	var token model.Token
	require.NoError(t, db.Where("managed_by = ?", model.CaoliaoManagedBy).First(&token).Error)
	patchResponse := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(token.Id), `{"status":"disabled"}`)
	assert.Equal(t, http.StatusOK, patchResponse.Code)
	assert.NotContains(t, patchResponse.Body.String(), `"secret"`)
	assert.NotContains(t, patchResponse.Body.String(), token.Key)
	assert.Contains(t, patchResponse.Body.String(), `"status":"disabled"`)

	extendExpiryBody := `{"expires_at":` + strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10) + `}`
	extendExpiryResponse := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(token.Id), extendExpiryBody)
	assert.Equal(t, http.StatusOK, extendExpiryResponse.Code)
	assert.Contains(t, extendExpiryResponse.Body.String(), `"status":"disabled"`)
}

func TestCaoliaoTokenQuotaMinusOneMapsToUnlimitedQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open("file:caoliao-unlimited-quota-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	t.Cleanup(func() { model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLogDB, previousRedisEnabled })

	_, err = model.EnsureCaoliaoEmployee("E-UNLIMITED", "Unlimited Employee")
	require.NoError(t, err)
	expiresAt := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	createResponse := caoliaoContractRequest(http.MethodPost, "/employees/E-UNLIMITED/keys",
		`{"name":"Unlimited Key","token_quota":-1,"requests_per_minute":20,"tokens_per_minute":5000,"expires_at":`+expiresAt+`}`)
	require.Equal(t, http.StatusOK, createResponse.Code)
	assert.Contains(t, createResponse.Body.String(), `"token_quota":-1`)
	assert.Contains(t, createResponse.Body.String(), `"requests_per_two_hours":7200`)
	assert.Contains(t, createResponse.Body.String(), `"tokens_per_two_hours":12000000`)
	assert.Contains(t, createResponse.Body.String(), `"daily_token_quota":144000000`)
	assert.NotContains(t, createResponse.Body.String(), `"requests_per_minute"`)
	assert.NotContains(t, createResponse.Body.String(), `"tokens_per_minute"`)

	var token model.Token
	require.NoError(t, db.Where("managed_by = ?", model.CaoliaoManagedBy).First(&token).Error)
	assert.True(t, token.UnlimitedQuota)
	assert.Equal(t, 0, token.RemainQuota)

	require.NoError(t, db.Model(&token).Update("used_quota", 250).Error)
	finiteResponse := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(token.Id), `{"token_quota":1000}`)
	require.Equal(t, http.StatusOK, finiteResponse.Code)
	assert.Contains(t, finiteResponse.Body.String(), `"token_quota":1000`)
	require.NoError(t, db.First(&token, token.Id).Error)
	assert.False(t, token.UnlimitedQuota)
	assert.Equal(t, 750, token.RemainQuota)

	unlimitedResponse := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(token.Id), `{"token_quota":-1}`)
	require.Equal(t, http.StatusOK, unlimitedResponse.Code)
	assert.Contains(t, unlimitedResponse.Body.String(), `"token_quota":-1`)
	require.NoError(t, db.First(&token, token.Id).Error)
	assert.True(t, token.UnlimitedQuota)

	for _, quota := range []string{"0", "-2", strconv.FormatInt(int64(common.MaxQuota)+1, 10)} {
		response := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(token.Id), `{"token_quota":`+quota+`}`)
		assert.Equal(t, http.StatusBadRequest, response.Code, quota)
	}
}

func TestCaoliaoUpstreamErrorLogDoesNotPersistSensitiveMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousErrorLogEnabled := constant.ErrorLogEnabled
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open("file:caoliao-error-log-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	constant.ErrorLogEnabled = true
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		constant.ErrorLogEnabled = previousErrorLogEnabled
		common.RedisEnabled = previousRedisEnabled
	})

	require.NoError(t, db.Create(&model.User{Id: 991, Username: "managed-error-user"}).Error)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 991)
	ctx.Set("token_id", 1991)
	ctx.Set("token_name", "managed-key")
	ctx.Set("original_model", "local-model")
	ctx.Set("channel_id", 2991)
	common.SetContextKey(ctx, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)

	const sensitive = "prompt: board acquisition secret; upstream echoed private response"
	apiErr := relaykittypes.NewErrorWithStatusCode(errors.New(sensitive), relaykittypes.ErrorCode("upstream_failure"), http.StatusBadGateway)
	processChannelError(ctx, relaykittypes.ChannelError{ChannelId: 2991}, apiErr)

	var log model.Log
	require.NoError(t, db.First(&log).Error)
	assert.Equal(t, "managed upstream error: status=502 code=upstream_failure", log.Content)
	assert.NotContains(t, log.Content, sensitive)
	assert.NotContains(t, log.Other, sensitive)
}

func TestRelayErrorForInternalUseRedactsManagedOnly(t *testing.T) {
	const sensitive = "sensitive upstream prompt and response"
	apiErr := relaykittypes.NewErrorWithStatusCode(
		errors.New(sensitive),
		relaykittypes.ErrorCode("upstream_failure"),
		http.StatusBadGateway,
	)

	managed, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(managed, constant.ContextKeyTokenManagedBy, model.CaoliaoManagedBy)
	managedMessage := relayErrorForInternalUse(managed, apiErr)
	assert.Equal(t, "managed upstream error: status=502 code=upstream_failure", managedMessage)
	assert.NotContains(t, managedMessage, sensitive)

	regular, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Contains(t, relayErrorForInternalUse(regular, apiErr), sensitive)
}

func TestCaoliaoKeyEndpointsCannotManageOrdinaryTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open("file:caoliao-scope-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	t.Cleanup(func() { model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLogDB, previousRedisEnabled })

	ordinary := model.Token{
		UserId:       1,
		Key:          "ordinary-full-key-must-not-leak",
		Name:         "ordinary",
		Status:       common.TokenStatusEnabled,
		CreatedTime:  common.GetTimestamp(),
		AccessedTime: common.GetTimestamp(),
		ExpiredTime:  -1,
		RemainQuota:  1000,
	}
	require.NoError(t, ordinary.Insert())

	patchResponse := caoliaoContractRequest(http.MethodPatch, "/keys/"+strconv.Itoa(ordinary.Id), `{"status":"disabled"}`)
	assert.Equal(t, http.StatusNotFound, patchResponse.Code)
	deleteResponse := caoliaoContractRequest(http.MethodDelete, "/keys/"+strconv.Itoa(ordinary.Id), "")
	assert.Equal(t, http.StatusNotFound, deleteResponse.Code)

	var reloaded model.Token
	require.NoError(t, db.First(&reloaded, ordinary.Id).Error)
	assert.Equal(t, common.TokenStatusEnabled, reloaded.Status)
	assert.Equal(t, ordinary.Key, reloaded.Key)
}

func caoliaoContractRequest(method string, path string, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/employees/:employee_id/keys", PostCaoliaoEmployeeKey)
	router.GET("/employees/:employee_id/keys", GetCaoliaoEmployeeKeys)
	router.PATCH("/keys/:id", PatchCaoliaoKey)
	router.DELETE("/keys/:id", DeleteCaoliaoKey)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
