package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaoliaoIntegrationAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const validToken = "0123456789abcdef0123456789abcdef"

	t.Run("fails closed when no secret is configured", func(t *testing.T) {
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN", "")
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN_FILE", "")
		response := performCaoliaoAuthRequest(t, "Bearer "+validToken)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	})

	t.Run("accepts the environment secret and rejects a different token", func(t *testing.T) {
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN", validToken)
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN_FILE", "")

		accepted := performCaoliaoAuthRequest(t, "Bearer "+validToken)
		assert.Equal(t, http.StatusNoContent, accepted.Code)
		assert.Equal(t, "no-store", accepted.Header().Get("Cache-Control"))

		rejected := performCaoliaoAuthRequest(t, "Bearer 0123456789abcdef0123456789abcdee")
		assert.Equal(t, http.StatusUnauthorized, rejected.Code)
		for _, authorization := range []string{"", "Basic " + validToken, "Bearer", "Bearer " + validToken + " extra"} {
			malformed := performCaoliaoAuthRequest(t, authorization)
			assert.Equal(t, http.StatusUnauthorized, malformed.Code, authorization)
		}
	})

	t.Run("file secret takes precedence and tolerates a trailing newline", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "caoliao-token")
		require.NoError(t, os.WriteFile(secretPath, []byte(validToken+"\n"), 0o600))
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN_FILE", secretPath)

		response := performCaoliaoAuthRequest(t, "bearer "+validToken)
		assert.Equal(t, http.StatusNoContent, response.Code)
	})

	t.Run("fails closed when the configured secret file cannot be read", func(t *testing.T) {
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN", validToken)
		t.Setenv("CAOLIAO_INTEGRATION_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-token"))

		response := performCaoliaoAuthRequest(t, "Bearer "+validToken)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.NotContains(t, response.Body.String(), validToken)
	})
}

func performCaoliaoAuthRequest(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(CaoliaoIntegrationAuth())
	router.GET("/private", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
