package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCenterLegacyPrepareClonesOnlyOneModelWithoutReturningCredential(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	oldMapping := `{"deepseek-flash":"deepseek-chat","deepseek-v4-pro":"deepseek-reasoner"}`
	old := model.Channel{Name: "legacy-shared", Type: 43, Key: "never-return-this-supplier-key", Status: 1,
		Models: "deepseek-flash,deepseek-v4-pro", Group: "default", ModelMapping: &oldMapping}
	require.NoError(t, db.Create(&old).Error)
	for _, id := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: id, ChannelId: old.Id, Enabled: true}).Error)
	}
	before, status := centerModelOpsSnapshotData("deepseek-flash")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
		"requestId": "legacy_prepare_test_request_001", "expectedGatewayRevision": before["revision"]}
	code, response := callCenterBilling(t, input, CenterModelOpsPrepareLegacy)
	require.Equal(t, http.StatusOK, code, response)
	encoded, err := common.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), old.Key)
	var source model.Channel
	require.NoError(t, db.Where("models = ? AND id <> ?", "deepseek-flash", old.Id).Take(&source).Error)
	assert.Equal(t, old.Key, source.Key)
	assert.JSONEq(t, `{"deepseek-flash":"deepseek-chat"}`, source.GetModelMapping())
	var original model.Channel
	require.NoError(t, db.First(&original, old.Id).Error)
	assert.Equal(t, old.Models, original.Models)
	assert.Equal(t, oldMapping, original.GetModelMapping())
	code, response = callCenterBilling(t, input, CenterModelOpsPrepareLegacy)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["requestId"] = "legacy_prepare_test_request_002"
	code, _ = callCenterBilling(t, input, CenterModelOpsPrepareLegacy)
	assert.Equal(t, http.StatusConflict, code)
}

func TestCenterLegacyPrepareRejectsUnexpectedRouteAndModel(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	old := model.Channel{Name: "not-shared", Type: 43, Key: "secret", Status: 1,
		Models: "deepseek-flash,unrelated-model", Group: "default"}
	require.NoError(t, db.Create(&old).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "deepseek-flash", ChannelId: old.Id, Enabled: true}).Error)
	before, status := centerModelOpsSnapshotData("deepseek-flash")
	require.Equal(t, http.StatusOK, status)
	code, _ := callCenterBilling(t, map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
		"requestId": "legacy_prepare_test_request_003", "expectedGatewayRevision": before["revision"]}, CenterModelOpsPrepareLegacy)
	assert.Equal(t, http.StatusConflict, code)
	code, _ = callCenterBilling(t, map[string]any{"modelId": "another-model", "legacyChannelId": old.Id,
		"requestId": "legacy_prepare_test_request_004", "expectedGatewayRevision": before["revision"]}, CenterModelOpsPrepareLegacy)
	assert.Equal(t, http.StatusBadRequest, code)
	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestCenterLegacyCutoverMovesOneModelAtATime(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	catalogKey := "model-test-catalog-key-00000002"
	require.NoError(t, db.Create(&model.Token{Name: "control-catalog-read", Key: catalogKey,
		Status: 1, Group: "default", ModelLimitsEnabled: true,
		ModelLimits: "deepseek-flash,deepseek-v4-pro", ExpiredTime: -1}).Error)
	mapping := `{"deepseek-flash":"deepseek-chat","deepseek-v4-pro":"deepseek-reasoner"}`
	old := model.Channel{Name: "old-shared", Type: 43, Key: "supplier-secret", Status: 1,
		Models: "deepseek-flash,deepseek-v4-pro", Group: "default", ModelMapping: &mapping}
	require.NoError(t, db.Create(&old).Error)
	for _, id := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: id, ChannelId: old.Id, Enabled: true}).Error)
	}
	var lastInternalID, lastPublicID int
	var lastRevision string
	for i, id := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		before, status := centerModelOpsSnapshotData(id)
		require.Equal(t, http.StatusOK, status)
		code, response := callCenterBilling(t, map[string]any{"modelId": id, "legacyChannelId": old.Id,
			"requestId": "legacy_cutover_prepare_000" + string(rune('1'+i)), "expectedGatewayRevision": before["revision"]}, CenterModelOpsPrepareLegacy)
		require.Equal(t, http.StatusOK, code, response)
		internalID := int(response["data"].(map[string]any)["channelId"].(float64))
		preparedRevision := response["data"].(map[string]any)["revision"]
		code, response = callCenterBilling(t, map[string]any{"modelId": id, "sourceChannelId": internalID,
			"legacyChannelId": old.Id, "catalogToken": "sk-" + catalogKey,
			"requestId": "legacy_cutover_stage_000" + string(rune('1'+i)), "expectedGatewayRevision": preparedRevision}, CenterModelOpsStagePublicRoute)
		require.Equal(t, http.StatusOK, code, response)
		var catalog model.Token
		require.NoError(t, db.Where("name = ?", "control-catalog-read").Take(&catalog).Error)
		assert.Equal(t, "deepseek-flash,deepseek-v4-pro", catalog.ModelLimits)
		staged := response["data"].(map[string]any)
		publicID := int(staged["publicChannelId"].(float64))
		require.NoError(t, db.First(&old, old.Id).Error)
		assert.Equal(t, common.ChannelStatusEnabled, old.Status)
		assert.Contains(t, old.GetModels(), id)
		code, response = callCenterBilling(t, map[string]any{"modelId": id, "sourceChannelId": internalID,
			"publicChannelId": publicID, "legacyChannelId": old.Id,
			"requestId": "legacy_cutover_activate_000" + string(rune('1'+i)), "expectedGatewayRevision": staged["revision"]}, CenterModelOpsActivatePublicRoute)
		require.Equal(t, http.StatusOK, code, response)
		lastInternalID, lastPublicID = internalID, publicID
		lastRevision = response["data"].(map[string]any)["revision"].(string)
		require.NoError(t, db.First(&old, old.Id).Error)
		assert.NotContains(t, old.GetModels(), id)
		if i == 0 {
			assert.Equal(t, []string{"deepseek-v4-pro"}, old.GetModels())
			assert.Equal(t, common.ChannelStatusEnabled, old.Status)
		} else {
			assert.Empty(t, old.GetModels())
			assert.Equal(t, common.ChannelStatusManuallyDisabled, old.Status)
		}
		var public model.Channel
		require.NoError(t, db.First(&public, publicID).Error)
		assert.Equal(t, common.ChannelStatusEnabled, public.Status)
		var oldAbility model.Ability
		require.NoError(t, db.Where("channel_id = ? AND model = ?", old.Id, id).Take(&oldAbility).Error)
		assert.False(t, oldAbility.Enabled)
	}
	code, response := callCenterBilling(t, map[string]any{"modelId": "deepseek-v4-pro", "legacyChannelId": old.Id,
		"sourceChannelId": lastInternalID, "publicChannelId": lastPublicID, "action": "rollback",
		"requestId": "legacy_cutover_rollback_001", "expectedGatewayRevision": lastRevision}, CenterModelOpsRecoverLegacy)
	require.Equal(t, http.StatusOK, code, response)
	require.NoError(t, db.First(&old, old.Id).Error)
	assert.Equal(t, []string{"deepseek-v4-pro"}, old.GetModels())
	assert.Equal(t, common.ChannelStatusEnabled, old.Status)
}

func TestCenterLegacyAbortAndRollbackPreserveOldRoute(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	catalogKey := "model-test-catalog-key-00000003"
	require.NoError(t, db.Create(&model.Token{Name: "control-catalog-read", Key: catalogKey,
		Status: 1, Group: "default", ModelLimitsEnabled: true,
		ModelLimits: "deepseek-flash,deepseek-v4-pro", ExpiredTime: -1}).Error)
	old := model.Channel{Name: "shared-before-recovery", Type: 43, Key: "supplier-secret", Status: 1,
		Models: "deepseek-flash,deepseek-v4-pro", Group: "default"}
	require.NoError(t, db.Create(&old).Error)
	for _, id := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: id, ChannelId: old.Id, Enabled: true}).Error)
	}
	before, status := centerModelOpsSnapshotData("deepseek-flash")
	require.Equal(t, http.StatusOK, status)
	code, response := callCenterBilling(t, map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
		"requestId": "legacy_recovery_prepare_001", "expectedGatewayRevision": before["revision"]}, CenterModelOpsPrepareLegacy)
	require.Equal(t, http.StatusOK, code, response)
	internalID := int(response["data"].(map[string]any)["channelId"].(float64))
	for i, action := range []string{"abort", "rollback"} {
		before, status = centerModelOpsSnapshotData("deepseek-flash")
		require.Equal(t, http.StatusOK, status)
		stageRequest := "legacy_recovery_stage_00" + string(rune('1'+i))
		code, response = callCenterBilling(t, map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
			"sourceChannelId": internalID, "catalogToken": "sk-" + catalogKey,
			"requestId": stageRequest, "expectedGatewayRevision": before["revision"]}, CenterModelOpsStagePublicRoute)
		require.Equal(t, http.StatusOK, code, response)
		publicID := int(response["data"].(map[string]any)["publicChannelId"].(float64))
		if action == "rollback" {
			code, response = callCenterBilling(t, map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
				"sourceChannelId": internalID, "publicChannelId": publicID,
				"requestId": "legacy_recovery_activate_001", "expectedGatewayRevision": response["data"].(map[string]any)["revision"]}, CenterModelOpsActivatePublicRoute)
			require.Equal(t, http.StatusOK, code, response)
		}
		recoveryInput := map[string]any{"modelId": "deepseek-flash", "legacyChannelId": old.Id,
			"sourceChannelId": internalID, "publicChannelId": publicID, "action": action,
			"requestId": "legacy_recovery_" + action + "_001", "expectedGatewayRevision": response["data"].(map[string]any)["revision"]}
		code, response = callCenterBilling(t, recoveryInput, CenterModelOpsRecoverLegacy)
		require.Equal(t, http.StatusOK, code, response)
		code, response = callCenterBilling(t, recoveryInput, CenterModelOpsRecoverLegacy)
		require.Equal(t, http.StatusOK, code, response)
		assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
		require.NoError(t, db.First(&old, old.Id).Error)
		assert.Equal(t, common.ChannelStatusEnabled, old.Status)
		assert.ElementsMatch(t, []string{"deepseek-flash", "deepseek-v4-pro"}, old.GetModels())
		var count int64
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", publicID).Count(&count).Error)
		assert.Zero(t, count)
	}
}
