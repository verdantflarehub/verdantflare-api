package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveSD2IdempotencyKey(t *testing.T) {
	const requestID = "12b945e8-25fe-4a2b-8d06-79472c095f31"

	value, keyErr := ResolveSD2IdempotencyKey(requestID, "")
	require.Nil(t, keyErr)
	require.Equal(t, requestID, value)

	value, keyErr = ResolveSD2IdempotencyKey("", requestID)
	require.Nil(t, keyErr)
	require.Equal(t, requestID, value)

	value, keyErr = ResolveSD2IdempotencyKey(stringsUpper(requestID), requestID)
	require.Nil(t, keyErr)
	require.Equal(t, requestID, value)

	_, keyErr = ResolveSD2IdempotencyKey("", "")
	require.Equal(t, "missing_idempotency_key", keyErr.Code)

	_, keyErr = ResolveSD2IdempotencyKey("not-a-uuid", "")
	require.Equal(t, "invalid_idempotency_key", keyErr.Code)

	_, keyErr = ResolveSD2IdempotencyKey(requestID, "486a1702-af73-4f3f-853a-0d202c96111a")
	require.Equal(t, "conflicting_idempotency_keys", keyErr.Code)
}

func TestIsSD2CreateAlias(t *testing.T) {
	require.True(t, isSD2CreateAlias("POST", "/v1/videos"))
	require.True(t, isSD2CreateAlias("POST", "/v1/video/generations"))
	require.False(t, isSD2CreateAlias("GET", "/v1/videos"))
	require.False(t, isSD2CreateAlias("POST", "/v1/videos/task_1/remix"))
}

func TestSD2SubmissionIdempotencyDarkReleaseGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := `{"model":"verdantflare-sd2","prompt":"test"}`

	t.Setenv("SD2_SUBMISSION_LEDGER_ENABLED", "false")
	router := gin.New()
	router.Use(SD2SubmissionIdempotency())
	router.POST("/v1/videos", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code)

	t.Setenv("SD2_SUBMISSION_LEDGER_ENABLED", "true")
	t.Setenv(service.SD2RequestIdentityHMACKeyEnv, "0123456789abcdef0123456789abcdef")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "missing_idempotency_key")
}

func TestSD2SubmissionIdempotencyRejectsUnknownFieldsBeforeRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(service.SD2SubmissionLedgerEnv, "true")
	t.Setenv(service.SD2RequestIdentityHMACKeyEnv, "0123456789abcdef0123456789abcdef")
	routed := 0
	router := gin.New()
	router.Use(SD2SubmissionIdempotency())
	router.POST("/v1/videos", func(c *gin.Context) {
		routed++
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"verdantflare-sd2","prompt":"p","provider":"wxmaas"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "12b945e8-25fe-4a2b-8d06-79472c095f31")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "unsupported_provider_field")
	require.Zero(t, routed)
}

func TestSD2SubmissionIdempotencyFailsClosedWithoutIdentityKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(service.SD2SubmissionLedgerEnv, "true")
	t.Setenv(service.SD2RequestIdentityHMACKeyEnv, "")
	routed := 0
	router := gin.New()
	router.Use(SD2SubmissionIdempotency())
	router.POST("/v1/videos", func(c *gin.Context) {
		routed++
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"verdantflare-sd2","prompt":"p"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "12b945e8-25fe-4a2b-8d06-79472c095f31")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "submission_identity_unavailable")
	require.Zero(t, routed)
}

func stringsUpper(value string) string {
	result := []byte(value)
	for index, char := range result {
		if char >= 'a' && char <= 'f' {
			result[index] = char - ('a' - 'A')
		}
	}
	return string(result)
}
