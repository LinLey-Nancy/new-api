package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeManagedLimitResponsesPreserveStableCode(t *testing.T) {
	tests := []struct {
		kind common.ManagedUsageLimitKind
		code types.ErrorCode
	}{
		{common.ManagedUsageLimitRequests2H, middleware.CaoliaoRequestsTwoHourLimitCode},
		{common.ManagedUsageLimitTokens2H, middleware.CaoliaoOutputTwoHourLimitCode},
		{common.ManagedUsageLimitTokensDaily, middleware.CaoliaoOutputDailyLimitCode},
	}

	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			const retryAfter = int64(3_600)
			code, message := middleware.ManagedUsageLimitError(test.kind, retryAfter)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Header("Retry-After", strconv.FormatInt(retryAfter, 10))
			apiError := types.NewErrorWithStatusCode(
				errors.New(message),
				code,
				http.StatusTooManyRequests,
				types.ErrOptionWithSkipRetry(),
			)

			ctx.JSON(apiError.StatusCode, gin.H{
				"type":  "error",
				"error": apiError.ToClaudeError(),
			})

			assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
			assert.Equal(t, "3600", recorder.Header().Get("Retry-After"))
			require.JSONEq(t,
				`{"type":"error","error":{"type":"new_api_error","message":"`+message+`","code":"`+string(test.code)+`"}}`,
				recorder.Body.String(),
			)
		})
	}
}
