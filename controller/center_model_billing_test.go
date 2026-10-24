package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func callCenterBilling(t *testing.T, body any, handler gin.HandlerFunc) (int, map[string]any) {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/internal/center/model-ops/billing/apply", strings.NewReader(string(encoded)))
	handler(context)
	var response map[string]any
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return recorder.Code, response
}

func TestCenterModelBillingScopedWriteReplayAndConflict(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	modelID := "internal-billing-test"
	otherID := "unrelated-existing-model"
	require.NoError(t, db.Create(&model.Option{Key: "ModelPrice", Value: `{"unrelated-existing-model":2.5}`}).Error)
	model.InitOptionMap()
	initial, status := centerModelOpsSnapshotData(modelID)
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{
		"modelId": modelID, "requestId": "billing_test_request_0001",
		"expectedGatewayRevision": initial["revision"], "mode": "per_request", "priceUsd": 0.02,
	}
	code, response := callCenterBilling(t, input, CenterModelOpsBillingApply)
	require.Equal(t, http.StatusOK, code, response)
	data := response["data"].(map[string]any)
	assert.Equal(t, false, data["replayed"])
	pricing := data["pricing"].(map[string]any)
	assert.Equal(t, 0.02, pricing["modelPrice"])
	var option model.Option
	require.NoError(t, db.Where("key = ?", "ModelPrice").Take(&option).Error)
	var prices map[string]float64
	require.NoError(t, common.Unmarshal([]byte(option.Value), &prices))
	assert.Equal(t, 2.5, prices[otherID])
	assert.Equal(t, 0.02, prices[modelID])

	code, response = callCenterBilling(t, input, CenterModelOpsBillingApply)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])

	input["requestId"] = "billing_test_request_0002"
	code, _ = callCenterBilling(t, input, CenterModelOpsBillingApply)
	assert.Equal(t, http.StatusConflict, code)
	input["requestId"] = "billing_test_request_0001"
	input["priceUsd"] = 0.03
	code, _ = callCenterBilling(t, input, CenterModelOpsBillingApply)
	assert.Equal(t, http.StatusConflict, code)

	current, status := centerModelOpsSnapshotData(modelID)
	require.Equal(t, http.StatusOK, status)
	code, response = callCenterBilling(t, map[string]any{
		"modelId": modelID, "requestId": "billing_test_request_0003",
		"expectedGatewayRevision": current["revision"], "mode": "tiered_expr",
		"billingExpr": `tier("base", p * 2 + c * 4)`,
	}, CenterModelOpsBillingApply)
	require.Equal(t, http.StatusOK, code, response)
	tiered := response["data"].(map[string]any)["pricing"].(map[string]any)
	assert.Equal(t, "tiered_expr", tiered["billingMode"])
	assert.NotContains(t, tiered, "modelPrice")
	require.NoError(t, db.Where("key = ?", "ModelPrice").Take(&option).Error)
	prices = nil
	require.NoError(t, common.Unmarshal([]byte(option.Value), &prices))
	assert.Equal(t, 2.5, prices[otherID])
	assert.NotContains(t, prices, modelID)
}

func TestCenterModelBillingRejectsInvalidExpressionAndUnknownFields(t *testing.T) {
	setupModelListControllerTestDB(t)
	for _, input := range []map[string]any{
		{"modelId": "internal-test", "mode": "tiered_expr", "billingExpr": "p * -2"},
		{"modelId": "internal-test", "mode": "per_request", "priceUsd": -1.0},
		{"modelId": "internal-test", "mode": "per_request", "priceUsd": 1.0, "secret": "must-not-pass"},
	} {
		code, _ := callCenterBilling(t, input, CenterModelOpsBillingPreview)
		assert.Equal(t, http.StatusBadRequest, code)
	}
}

func TestCenterModelBillingFirstWritePreservesGatewayDefaultPrices(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	ratio_setting.InitRatioSettings()
	model.InitOptionMap()
	modelID := "internal-first-price"
	before, status := centerModelOpsSnapshotData(modelID)
	require.Equal(t, http.StatusOK, status)
	code, response := callCenterBilling(t, map[string]any{
		"modelId": modelID, "requestId": "billing_test_request_0004",
		"expectedGatewayRevision": before["revision"], "mode": "per_request", "priceUsd": 0.01,
	}, CenterModelOpsBillingApply)
	require.Equal(t, http.StatusOK, code, response)
	var option model.Option
	require.NoError(t, db.Where("key = ?", "ModelPrice").Take(&option).Error)
	var prices map[string]float64
	require.NoError(t, common.Unmarshal([]byte(option.Value), &prices))
	assert.Equal(t, 0.04, prices["dall-e-3"])
	assert.Equal(t, 0.01, prices[modelID])
}
