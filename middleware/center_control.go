package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// CenterControlAuth protects the narrow service-to-service API. It never
// accepts browser sessions or ordinary model API tokens as an alternative.
func CenterControlAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := strings.TrimSpace(os.Getenv("CENTER_CONTROL_TOKEN"))
		if len(expected) < 32 {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		authorization := c.GetHeader("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		provided := strings.TrimPrefix(authorization, "Bearer ")
		want, got := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}
