package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCenterModelMetadataScopedWriteAndReplay(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	other := model.Model{ModelName: "existing-unrelated", Description: "must remain untouched", Status: 1}
	require.NoError(t, db.Create(&other).Error)
	before, status := centerModelOpsSnapshotData("internal-deepseek-test")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{
		"modelId": "internal-deepseek-test", "description": "Internal DeepSeek channel test model.",
		"endpoints": "/v1/chat/completions", "status": 0,
		"requestId": "metadata_request_test_0001", "expectedGatewayRevision": before["revision"],
	}
	code, response := callCenterBilling(t, input, CenterModelOpsConfigureMetadata)
	require.Equal(t, http.StatusOK, code, response)
	var saved model.Model
	require.NoError(t, db.Where("model_name = ?", "internal-deepseek-test").Take(&saved).Error)
	assert.Zero(t, saved.Status)
	assert.Zero(t, saved.SyncOfficial)
	require.NoError(t, db.Where("model_name = ?", other.ModelName).Take(&other).Error)
	assert.Equal(t, "must remain untouched", other.Description)
	code, response = callCenterBilling(t, input, CenterModelOpsConfigureMetadata)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["requestId"] = "metadata_request_test_0002"
	code, _ = callCenterBilling(t, input, CenterModelOpsConfigureMetadata)
	assert.Equal(t, http.StatusConflict, code)
}
