package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCenterPublicRouteStaysDisabledUntilSeparateActivation(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	catalogKey := "model-test-catalog-key-00000001"
	require.NoError(t, db.Create(&model.Token{Name: "control-catalog-read", Key: catalogKey,
		Status: 1, Group: "default", ModelLimitsEnabled: true, ModelLimits: "existing-model", ExpiredTime: -1}).Error)
	mapping := `{"internal-model":"deepseek-chat"}`
	source := model.Channel{Name: "internal-source", Type: 43, Key: "upstream-secret", Status: 1,
		Models: "internal-model", Group: "center-internal", ModelMapping: &mapping}
	require.NoError(t, db.Create(&source).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "center-internal", Model: "internal-model", ChannelId: source.Id, Enabled: true}).Error)
	before, status := centerModelOpsSnapshotData("internal-model")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{"modelId": "internal-model", "sourceChannelId": source.Id,
		"catalogToken": "sk-" + catalogKey, "requestId": "public_stage_request_0001", "expectedGatewayRevision": before["revision"]}
	badInput := map[string]any{"modelId": "internal-model", "sourceChannelId": source.Id,
		"catalogToken": "sk-unknown-customer-token-00000001", "requestId": "public_stage_invalid_0001", "expectedGatewayRevision": before["revision"]}
	badCode, _ := callCenterBilling(t, badInput, CenterModelOpsStagePublicRoute)
	require.Equal(t, http.StatusConflict, badCode)
	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Where("models = ?", "internal-model").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	code, response := callCenterBilling(t, input, CenterModelOpsStagePublicRoute)
	require.Equal(t, http.StatusOK, code, response)
	stage := response["data"].(map[string]any)
	publicID := int(stage["publicChannelId"].(float64))
	var public model.Channel
	require.NoError(t, db.First(&public, publicID).Error)
	assert.Equal(t, "default", public.Group)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, public.Status)
	assert.Equal(t, source.Key, public.Key)
	var ability model.Ability
	require.NoError(t, db.Where("channel_id = ?", publicID).Take(&ability).Error)
	assert.False(t, ability.Enabled)
	var catalog model.Token
	require.NoError(t, db.Where("name = ?", "control-catalog-read").Take(&catalog).Error)
	assert.Equal(t, "existing-model,internal-model", catalog.ModelLimits)
	code, response = callCenterBilling(t, input, CenterModelOpsStagePublicRoute)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	activate := map[string]any{"modelId": "internal-model", "sourceChannelId": source.Id,
		"publicChannelId": publicID, "requestId": "public_activate_request_001",
		"expectedGatewayRevision": stage["revision"]}
	code, response = callCenterBilling(t, activate, CenterModelOpsActivatePublicRoute)
	require.Equal(t, http.StatusOK, code, response)
	require.NoError(t, db.First(&public, publicID).Error)
	assert.Equal(t, common.ChannelStatusEnabled, public.Status)
	require.NoError(t, db.Where("channel_id = ?", publicID).Take(&ability).Error)
	assert.True(t, ability.Enabled)
}
