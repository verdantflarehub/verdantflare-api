package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCenterModelOpsSnapshotExactModelAndCredentialRedaction(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	baseURL := "https://user:upstream-password@provider.example/v1?token=hidden"
	mapping := `{"test-model":"provider-model"}`
	channel := model.Channel{
		Name: "test-channel", Key: "supplier-secret", Models: "test-model,test-model-plus",
		Group: "default,trial", BaseURL: &baseURL, ModelMapping: &mapping,
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Channel{
		Name: "other-channel", Key: "another-secret", Models: "test-model-plus", Group: "default",
	}).Error)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/internal/center/model-ops/snapshot?modelId=test-model", nil)
	CenterModelOpsSnapshot(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.Contains(t, body, `"name":"test-channel"`)
	require.Contains(t, body, `"upstreamHost":"provider.example"`)
	require.Contains(t, body, `"credentialConfigured":true`)
	require.Contains(t, body, `"upstreamModel":"provider-model"`)
	for _, forbidden := range []string{"supplier-secret", "another-secret", "upstream-password", "token=hidden", "other-channel"} {
		require.NotContains(t, body, forbidden)
	}
	var original struct {
		Data struct {
			Revision string `json:"revision"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &original))
	require.Len(t, original.Data.Revision, 64)
	require.NoError(t, db.Model(&channel).Update("key", "rotated-supplier-secret").Error)
	rotated := httptest.NewRecorder()
	rotatedContext, _ := gin.CreateTestContext(rotated)
	rotatedContext.Request = httptest.NewRequest(http.MethodGet, "/api/internal/center/model-ops/snapshot?modelId=test-model", nil)
	CenterModelOpsSnapshot(rotatedContext)
	var updated struct {
		Data struct {
			Revision string `json:"revision"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rotated.Body.Bytes(), &updated))
	require.NotEqual(t, original.Data.Revision, updated.Data.Revision)
	require.NotContains(t, rotated.Body.String(), "rotated-supplier-secret")
}

func TestCenterModelOpsSnapshotRejectsInvalidModelID(t *testing.T) {
	for _, modelID := range []string{"", strings.Repeat("x", 129), "%0A"} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodGet, "/api/internal/center/model-ops/snapshot?modelId="+modelID, nil)
		CenterModelOpsSnapshot(context)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}
