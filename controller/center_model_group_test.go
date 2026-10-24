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

func TestCenterInternalGroupExplicitCreateReplayAndConflict(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	model.InitOptionMap()
	// The fixture's option state is controlled by the test rather than relying
	// on whatever group settings another test left in process memory.
	configureCenterInternalGroupForTest(t)
	groups := ratio_setting.GetGroupRatioCopy()
	delete(groups, "center-internal")
	// Removing the group through the existing settings parser restores a clean
	// pre-create state; persistence is initialized from this in-memory default.
	encoded, err := common.Marshal(groups)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(encoded)))
	require.NoError(t, db.Model(&model.Option{}).Where("key = ?", "GroupRatio").Update("value", string(encoded)).Error)
	before, exists, err := model.CenterInternalGroupSnapshot()
	require.NoError(t, err)
	require.False(t, exists)
	input := map[string]any{"group": "center-internal", "requestId": "group_create_request_0001", "expectedGroupRevision": before}
	code, response := callCenterBilling(t, input, CenterModelOpsGroupApply)
	require.Equal(t, http.StatusOK, code, response)
	assert.True(t, ratio_setting.ContainsGroupRatio("center-internal"))
	var option model.Option
	require.NoError(t, db.Where("key = ?", "GroupRatio").Take(&option).Error)
	assert.Contains(t, option.Value, `"center-internal":1`)
	code, response = callCenterBilling(t, input, CenterModelOpsGroupApply)
	require.Equal(t, http.StatusOK, code, response)
	assert.Equal(t, true, response["data"].(map[string]any)["replayed"])
	input["requestId"] = "group_create_request_0002"
	code, _ = callCenterBilling(t, input, CenterModelOpsGroupApply)
	assert.Equal(t, http.StatusConflict, code)
}
