package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
