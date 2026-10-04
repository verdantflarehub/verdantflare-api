package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCenterControlAuthFailsClosedAndRequiresServiceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/internal", CenterControlAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	call := func(header string) int {
		request := httptest.NewRequest(http.MethodGet, "/internal", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	t.Setenv("CENTER_CONTROL_TOKEN", "")
	require.Equal(t, http.StatusServiceUnavailable, call("Bearer any"))
	t.Setenv("CENTER_CONTROL_TOKEN", "test-service-token-with-at-least-32-characters")
	require.Equal(t, http.StatusUnauthorized, call(""))
	require.Equal(t, http.StatusUnauthorized, call("Bearer wrong"))
	require.Equal(t, http.StatusNoContent, call("Bearer test-service-token-with-at-least-32-characters"))
}
