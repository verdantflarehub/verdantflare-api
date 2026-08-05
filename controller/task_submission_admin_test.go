package controller

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactTaskSubmissionSnapshotRemovesPrivateRequestMaterial(t *testing.T) {
	raw := `{"schema_version":1,"duration":15,"prompt":"private prompt","image_url":"https://signed.example/a.png?token=secret","nested":{"credential":"api-secret","ratio":"16:9"}}`
	redacted := redactTaskSubmissionSnapshot(raw)

	assert.NotContains(t, redacted, "private prompt")
	assert.NotContains(t, redacted, "signed.example")
	assert.NotContains(t, redacted, "api-secret")
	assert.Contains(t, redacted, `"duration":15`)
	assert.Contains(t, redacted, `"ratio":"16:9"`)
}

func TestAdminTaskSubmissionListViewDoesNotMarshalPrivateFields(t *testing.T) {
	view := adminTaskSubmissionView{ID: 1, ClientRequestID: "request-safe", PublicTaskID: "task-safe"}
	data, err := common.Marshal(view)
	require.NoError(t, err)
	body := string(data)

	assert.Contains(t, body, "request-safe")
	for _, privateField := range []string{"credential", "request_digest", "upstream_task_id", "archived_result_ref"} {
		assert.False(t, strings.Contains(body, privateField), privateField)
	}
}
