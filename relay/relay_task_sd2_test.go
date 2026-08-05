package relay

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func TestCaptureSD2ProviderResponseEvidenceHashesRawBodyAndBoundsIt(t *testing.T) {
	t.Setenv(service.SD2SubmissionLedgerEnv, "true")
	info := &relaycommon.RelayInfo{OriginModelName: service.SD2OriginModel}
	response := &http.Response{Body: io.NopCloser(strings.NewReader(`{"task_id":"provider-task"}`))}
	result := &TaskSubmitResult{}

	captured, err := captureSD2ProviderResponseEvidence(info, response, result)
	require.NoError(t, err)
	require.True(t, captured)
	require.NotEmpty(t, result.ResponseDigest)
	restored, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"task_id":"provider-task"}`, string(restored))

	response = &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxSD2ProviderCreateResponseBytes+1)))}
	result = &TaskSubmitResult{}
	captured, err = captureSD2ProviderResponseEvidence(info, response, result)
	require.True(t, captured)
	require.ErrorContains(t, err, "too large")
	require.NotEmpty(t, result.ResponseDigest)
}

func TestConvertLedgerSD2ToOpenAIVideoDoesNotExposeProviderCostData(t *testing.T) {
	taskDBID := int64(42)
	generateAudio := false
	seed := int64(-1)
	snapshot := service.NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "succeeded"
	snapshot.Duration = 10
	snapshot.Ratio = "16:9"
	snapshot.Resolution = "720P"
	snapshot.FramesPerSecond = 24
	snapshot.GenerateAudio = &generateAudio
	snapshot.Seed = &seed
	snapshot.Usage = service.SD2TaskUsageSnapshot{CompletionTokens: 123, TotalTokens: 456}
	snapshot.ArchiveState = service.SD2ArchiveStateArchived
	archivedResult := service.SD2ArchivedResult{
		SchemaVersion: service.SD2ArchivedResultSchemaVersion,
		Ref:           "sd2-result://private-results/user-7/task_public/video.mp4",
		VersionID:     "version-1",
		SHA256:        strings.Repeat("ab", 32),
		Size:          1024,
		ContentType:   "video/mp4",
		ETag:          `"etag-1"`,
	}
	snapshot.ArchivedResult = &archivedResult
	data, err := service.EncodeSD2TaskResultSnapshot(snapshot)
	require.NoError(t, err)

	task := &model.Task{
		ID:         taskDBID,
		TaskID:     "task_public",
		UserId:     7,
		Status:     model.TaskStatusSuccess,
		Progress:   "100%",
		Data:       data,
		CreatedAt:  100,
		FinishTime: 200,
	}
	submission := &model.TaskSubmission{
		TaskDBID:                  &taskDBID,
		PublicTaskID:              task.TaskID,
		UserID:                    task.UserId,
		OriginModelName:           service.SD2OriginModel,
		State:                     model.TaskSubmissionStateConfirmed,
		BillingState:              model.TaskSubmissionBillingStateSettled,
		ArchiveState:              model.TaskSubmissionArchiveStateArchived,
		ArchiveSchemaVersion:      archivedResult.SchemaVersion,
		ArchivedResultRef:         archivedResult.Ref,
		ArchivedResultVersionID:   archivedResult.VersionID,
		ArchivedResultSHA256:      archivedResult.SHA256,
		ArchivedResultSize:        archivedResult.Size,
		ArchivedResultContentType: archivedResult.ContentType,
		ArchivedResultETag:        archivedResult.ETag,
	}

	body, err := convertLedgerSD2ToOpenAIVideo(task, submission)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"id":"task_public",
		"task_id":"task_public",
		"object":"video",
		"model":"verdantflare-sd2",
		"status":"completed",
		"progress":100,
		"created_at":100,
		"completed_at":200,
		"metadata":{
			"duration":10,
			"ratio":"16:9",
			"resolution":"720p",
			"framespersecond":24,
			"generate_audio":false,
			"seed":-1,
			"url":"http://localhost:3000/v1/videos/task_public/content"
		}
	}`, string(body))
	require.NotContains(t, string(body), "usage")
	require.NotContains(t, string(body), "provider_status")
	require.NotContains(t, string(body), "archived_result_ref")
	require.NotContains(t, string(body), "sd2-result://")
}

func TestConvertLedgerSD2ToOpenAIVideoFailsClosedBeforeArchiveSettlement(t *testing.T) {
	taskDBID := int64(43)
	snapshot := service.NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "succeeded"
	snapshot.ArchiveState = service.SD2ArchiveStatePending
	data, err := service.EncodeSD2TaskResultSnapshot(snapshot)
	require.NoError(t, err)

	task := &model.Task{ID: taskDBID, TaskID: "task_pending", UserId: 8, Status: model.TaskStatusSuccess, Data: data}
	submission := &model.TaskSubmission{
		TaskDBID:        &taskDBID,
		PublicTaskID:    task.TaskID,
		UserID:          task.UserId,
		OriginModelName: service.SD2OriginModel,
		State:           model.TaskSubmissionStateConfirmed,
		BillingState:    model.TaskSubmissionBillingStateCommitted,
		ArchiveState:    model.TaskSubmissionArchiveStatePending,
	}

	_, err = convertLedgerSD2ToOpenAIVideo(task, submission)
	require.ErrorContains(t, err, "not delivery-complete")
}
