package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCenterModelAliasAttachesExistingUpstreamWithoutExposingCredential(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	model.InitOptionMap()
	mapping := `{"deepseek-v4-pro":"deepseek-chat"}`
	source := model.Channel{Name: "test-source", Type: 43, Key: "never-return-this-secret",
		Status: 1, Models: "deepseek-v4-pro", Group: "center-internal", ModelMapping: &mapping}
	require.NoError(t, db.Create(&source).Error)
	before, status := centerModelOpsSnapshotData("internal-deepseek-alias")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{
		"modelId": "internal-deepseek-alias", "sourceModelId": "deepseek-v4-pro",
		"sourceChannelId": source.Id, "requestId": "alias_test_request_0001",
		"expectedGatewayRevision": before["revision"],
	}
	code, response := callCenterBilling(t, input, CenterModelOpsAttachAlias)
	require.Equal(t, http.StatusOK, code, response)
	responseJSON, err := common.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(responseJSON), "never-return-this-secret")
	var saved model.Channel
	require.NoError(t, db.First(&saved, source.Id).Error)
	assert.Equal(t, "deepseek-v4-pro,internal-deepseek-alias", saved.Models)
	assert.Contains(t, saved.GetModelMapping(), `"internal-deepseek-alias":"deepseek-chat"`)
	var ability model.Ability
	require.NoError(t, db.Where("model = ?", "internal-deepseek-alias").Take(&ability).Error)
	assert.Equal(t, "center-internal", ability.Group)

	code, response = callCenterBilling(t, input, CenterModelOpsAttachAlias)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["requestId"] = "alias_test_request_0002"
	code, _ = callCenterBilling(t, input, CenterModelOpsAttachAlias)
	assert.Equal(t, http.StatusConflict, code)
}

func TestCenterModelAliasRejectsSharedDefaultChannel(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	source := model.Channel{Name: "shared", Type: 43, Key: "secret", Status: 1,
		Models: "deepseek-v4-pro", Group: "default"}
	require.NoError(t, db.Create(&source).Error)
	before, status := centerModelOpsSnapshotData("internal-not-public")
	require.Equal(t, http.StatusOK, status)
	code, _ := callCenterBilling(t, map[string]any{
		"modelId": "internal-not-public", "sourceModelId": "deepseek-v4-pro",
		"sourceChannelId": source.Id, "requestId": "alias_test_request_0003",
		"expectedGatewayRevision": before["revision"],
	}, CenterModelOpsAttachAlias)
	assert.Equal(t, http.StatusConflict, code)
	var saved model.Channel
	require.NoError(t, db.First(&saved, source.Id).Error)
	assert.Equal(t, "deepseek-v4-pro", saved.Models)
}

func TestCenterModelChannelCreateInternalOnlyAndCredentialRedacted(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	model.InitOptionMap()
	configureCenterInternalGroupForTest(t)
	before, status := centerModelOpsSnapshotData("internal-deepseek-test")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{
		"modelId": "internal-deepseek-test", "upstreamModelId": "deepseek-chat",
		"name": "center-test-deepseek", "credential": "sk-secret-never-returned-12345",
		"requestId": "channel_create_test_request_0001", "expectedGatewayRevision": before["revision"],
	}
	code, response := callCenterBilling(t, input, CenterModelOpsCreateChannel)
	require.Equal(t, http.StatusOK, code, response)
	encoded, err := common.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), input["credential"])
	var saved model.Channel
	require.NoError(t, db.Where("models = ?", "internal-deepseek-test").Take(&saved).Error)
	assert.Equal(t, "center-internal", saved.Group)
	assert.Equal(t, "sk-secret-never-returned-12345", saved.Key)
	assert.Contains(t, saved.GetModelMapping(), `"internal-deepseek-test":"deepseek-chat"`)
	var ability model.Ability
	require.NoError(t, db.Where("model = ?", "internal-deepseek-test").Take(&ability).Error)
	assert.Equal(t, "center-internal", ability.Group)
	code, response = callCenterBilling(t, input, CenterModelOpsCreateChannel)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["credential"] = "sk-different-secret-1234567890"
	code, _ = callCenterBilling(t, input, CenterModelOpsCreateChannel)
	assert.Equal(t, http.StatusConflict, code)
}

func TestCenterModelChannelCreateRejectsStaleRevisionAndUnknownFields(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	configureCenterInternalGroupForTest(t)
	before, status := centerModelOpsSnapshotData("internal-deepseek-new")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{
		"modelId": "internal-deepseek-new", "upstreamModelId": "deepseek-chat",
		"name": "center-test-deepseek", "credential": "sk-secret-never-returned-12345",
		"requestId": "channel_create_test_request_0002", "expectedGatewayRevision": before["revision"],
		"group": "default",
	}
	code, _ := callCenterBilling(t, input, CenterModelOpsCreateChannel)
	assert.Equal(t, http.StatusBadRequest, code)
	delete(input, "group")
	input["expectedGatewayRevision"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	code, _ = callCenterBilling(t, input, CenterModelOpsCreateChannel)
	assert.Equal(t, http.StatusConflict, code)
	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Where("models = ?", "internal-deepseek-new").Count(&count).Error)
	assert.Zero(t, count)
}

func configureCenterInternalGroupForTest(t *testing.T) {
	t.Helper()
	original, err := common.Marshal(ratio_setting.GetGroupRatioCopy())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(original))) })
	groups := ratio_setting.GetGroupRatioCopy()
	groups["center-internal"] = 1
	configured, err := common.Marshal(groups)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(configured)))
	require.NoError(t, model.DB.Save(&model.Option{Key: "GroupRatio", Value: string(configured)}).Error)
}

func TestCenterModelChannelDisableRefusesSharedAndDisablesDedicatedRoute(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	shared := model.Channel{Name: "shared", Type: 43, Key: "secret", Status: 1, Models: "source-model", Group: "default"}
	require.NoError(t, db.Create(&shared).Error)
	before, status := centerModelOpsSnapshotData("source-model")
	require.Equal(t, http.StatusOK, status)
	code, _ := callCenterBilling(t, map[string]any{"modelId": "source-model", "channelId": shared.Id,
		"requestId": "disable_test_request_0001", "expectedGatewayRevision": before["revision"]}, CenterModelOpsDisableChannel)
	assert.Equal(t, http.StatusConflict, code)
	dedicated := model.Channel{Name: "dedicated", Type: 43, Key: "secret", Status: 1,
		Models: "internal-model", Group: "center-internal"}
	require.NoError(t, db.Create(&dedicated).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "center-internal", Model: "internal-model", ChannelId: dedicated.Id, Enabled: true}).Error)
	before, status = centerModelOpsSnapshotData("internal-model")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{"modelId": "internal-model", "channelId": dedicated.Id,
		"requestId": "disable_test_request_0002", "expectedGatewayRevision": before["revision"]}
	code, _ = callCenterBilling(t, input, CenterModelOpsDisableChannel)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, db.First(&dedicated, dedicated.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, dedicated.Status)
	var ability model.Ability
	require.NoError(t, db.Where("channel_id = ?", dedicated.Id).Take(&ability).Error)
	assert.False(t, ability.Enabled)
	code, response := callCenterBilling(t, input, CenterModelOpsDisableChannel)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
}

func TestCenterModelCredentialRotateIsDedicatedAndRedacted(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	dedicated := model.Channel{Name: "dedicated", Type: 43, Key: "old-secret-never-returned",
		Status: 1, Models: "internal-model", Group: "center-internal"}
	require.NoError(t, db.Create(&dedicated).Error)
	before, status := centerModelOpsSnapshotData("internal-model")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{"modelId": "internal-model", "channelId": dedicated.Id,
		"credential": "new-secret-never-returned", "requestId": "rotate_test_request_0001",
		"expectedGatewayRevision": before["revision"]}
	code, response := callCenterBilling(t, input, CenterModelOpsRotateCredential)
	require.Equal(t, http.StatusOK, code, response)
	encoded, err := common.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "new-secret-never-returned")
	require.NoError(t, db.First(&dedicated, dedicated.Id).Error)
	assert.Equal(t, "new-secret-never-returned", dedicated.Key)
	code, response = callCenterBilling(t, input, CenterModelOpsRotateCredential)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
}

func TestCenterModelChannelUpdateIsScopedAndIdempotent(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	dedicated := model.Channel{Name: "before", Type: 43, Key: "never-return-secret", Status: 1,
		Models: "internal-model", Group: "center-internal"}
	require.NoError(t, db.Create(&dedicated).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "center-internal", Model: "internal-model", ChannelId: dedicated.Id, Enabled: true}).Error)
	before, status := centerModelOpsSnapshotData("internal-model")
	require.Equal(t, http.StatusOK, status)
	input := map[string]any{"modelId": "internal-model", "channelId": dedicated.Id,
		"name": "after", "upstreamModelId": "deepseek-chat", "weight": 8, "priority": 2,
		"requestId": "channel_update_test_request_0001", "expectedGatewayRevision": before["revision"]}
	code, response := callCenterBilling(t, input, CenterModelOpsUpdateChannel)
	require.Equal(t, http.StatusOK, code, response)
	encoded, err := common.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "never-return-secret")
	require.NoError(t, db.First(&dedicated, dedicated.Id).Error)
	assert.Equal(t, "after", dedicated.Name)
	assert.Contains(t, dedicated.GetModelMapping(), `"internal-model":"deepseek-chat"`)
	assert.Equal(t, 8, dedicated.GetWeight())
	code, response = callCenterBilling(t, input, CenterModelOpsUpdateChannel)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["requestId"] = "channel_update_test_request_0002"
	code, _ = callCenterBilling(t, input, CenterModelOpsUpdateChannel)
	assert.Equal(t, http.StatusConflict, code)
	input["group"] = "default"
	code, _ = callCenterBilling(t, input, CenterModelOpsUpdateChannel)
	assert.Equal(t, http.StatusBadRequest, code)
}
