package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

const caoliaoIntegrationTokenMinLength = 32

// CaoliaoIntegrationAuth authenticates the private control-plane API used by
// Caoliaochang. The token is loaded on each request so a file-backed secret can
// be rotated without restarting the gateway.
func CaoliaoIntegrationAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		expectedToken, err := loadCaoliaoIntegrationToken()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"success": false,
				"message": "caoliao integration is not configured",
			})
			return
		}

		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "invalid integration credentials",
			})
			return
		}

		expectedHash := sha256.Sum256([]byte(expectedToken))
		providedHash := sha256.Sum256([]byte(parts[1]))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "invalid integration credentials",
			})
			return
		}

		c.Next()
	}
}

func loadCaoliaoIntegrationToken() (string, error) {
	if path := strings.TrimSpace(os.Getenv("CAOLIAO_INTEGRATION_TOKEN_FILE")); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(content))
		if len(token) < caoliaoIntegrationTokenMinLength {
			return "", errors.New("caoliao integration token is too short")
		}
		return token, nil
	}

	token := strings.TrimSpace(os.Getenv("CAOLIAO_INTEGRATION_TOKEN"))
	if len(token) < caoliaoIntegrationTokenMinLength {
		return "", errors.New("caoliao integration token is not configured or is too short")
	}
	return token, nil
}
