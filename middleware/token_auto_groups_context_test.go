package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTokenAutoGroupsContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	return ctx
}

func TestSetupContextForTokenPreservesCustomAutoGroupsOrder(t *testing.T) {
	ctx := newTokenAutoGroupsContext()
	token := &model.Token{Id: 1, UserId: 2, AutoGroups: `["vip","default"]`}

	require.NoError(t, SetupContextForToken(ctx, token))
	value, ok := common.GetContextKey(ctx, constant.ContextKeyTokenAutoGroups)
	require.True(t, ok)
	assert.Equal(t, []string{"vip", "default"}, value)
}

func TestSetupContextForTokenTreatsStoredEmptyArrayAsInheritance(t *testing.T) {
	ctx := newTokenAutoGroupsContext()
	token := &model.Token{Id: 1, UserId: 2, AutoGroups: `[]`}

	require.NoError(t, SetupContextForToken(ctx, token))
	_, ok := common.GetContextKey(ctx, constant.ContextKeyTokenAutoGroups)
	assert.False(t, ok)
}

func TestSetupContextForTokenMalformedAutoGroupsFailsClosed(t *testing.T) {
	ctx := newTokenAutoGroupsContext()
	token := &model.Token{Id: 1, UserId: 2, AutoGroups: `not-json`}

	require.NoError(t, SetupContextForToken(ctx, token))
	value, ok := common.GetContextKey(ctx, constant.ContextKeyTokenAutoGroups)
	require.True(t, ok)
	assert.Equal(t, []string{}, value)
}

func TestSetupContextForTokenCarriesCaoliaoManagedLimits(t *testing.T) {
	ctx := newTokenAutoGroupsContext()
	token := &model.Token{
		Id:                  81,
		UserId:              82,
		ManagedBy:           model.CaoliaoManagedBy,
		RequestsPerTwoHours: 23,
		TokensPerTwoHours:   45_678,
		DailyTokenQuota:     123_456,
	}

	require.NoError(t, SetupContextForToken(ctx, token))
	assert.Equal(t, model.CaoliaoManagedBy, common.GetContextKeyString(ctx, constant.ContextKeyTokenManagedBy))
	assert.Equal(t, 23, common.GetContextKeyInt(ctx, constant.ContextKeyTokenRequestsPerTwoHours))
	assert.Equal(t, 45_678, common.GetContextKeyInt(ctx, constant.ContextKeyTokenTokensPerTwoHours))
	assert.Equal(t, 123_456, common.GetContextKeyInt(ctx, constant.ContextKeyTokenDailyTokenQuota))
}
