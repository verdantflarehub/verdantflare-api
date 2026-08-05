package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSD2PollingLogsNeverContainProviderIdentifiersOrSignedURLs(t *testing.T) {
	var output bytes.Buffer
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultWriter
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultWriter = &output
	gin.DefaultErrorWriter = &output
	common.LogWriterMu.Unlock()
	previousDebug := common.DebugEnabled
	common.DebugEnabled = true
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = previousWriter
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
		common.DebugEnabled = previousDebug
	})

	const localTaskID = "task_public_safe"
	const upstreamTaskID = "provider_task_private"
	const signedURL = "https://result.example/video.mp4?signature=private"
	responseBody := []byte(`{"task_id":"` + upstreamTaskID + `","content":{"video_url":"` + signedURL + `"}}`)
	parsed := &relaycommon.TaskInfo{TaskID: upstreamTaskID, Status: model.TaskStatusSuccess, Url: signedURL}

	logVideoPollingResponse(context.Background(), true, http.StatusOK, localTaskID, responseBody)
	logVideoPollingParsed(context.Background(), true, localTaskID, "task_result", parsed)
	logVideoPollingUnrecognized(context.Background(), true, localTaskID, responseBody)
	logVideoPollingFailure(context.Background(), true, &model.Task{
		TaskID: localTaskID,
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: upstreamTaskID,
			ResultURL:      signedURL,
		},
		Data: responseBody,
	}, "provider failure containing "+signedURL)

	logs := output.String()
	require.Contains(t, logs, localTaskID)
	require.NotContains(t, logs, upstreamTaskID)
	require.NotContains(t, logs, signedURL)
	require.False(t, strings.Contains(logs, "signature=private"))
}

type fixedSD2PollingAdaptor struct {
	statusCode int
	body       []byte
	taskResult *relaycommon.TaskInfo
	parseCalls int
	fetchCalls int
	fetchedKey string
}

func (a *fixedSD2PollingAdaptor) Init(*relaycommon.RelayInfo) {}

func (a *fixedSD2PollingAdaptor) FetchTask(_ string, key string, _ map[string]any, _ string) (*http.Response, error) {
	a.fetchCalls++
	a.fetchedKey = key
	return &http.Response{
		StatusCode: a.statusCode,
		Body:       io.NopCloser(bytes.NewReader(a.body)),
	}, nil
}

func (a *fixedSD2PollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	a.parseCalls++
	if a.taskResult != nil {
		return a.taskResult, nil
	}
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *fixedSD2PollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

func seedConfirmedSD2LedgerTask(t *testing.T, userID int, tokenID int, requestID string) (*model.TaskSubmission, *model.Task) {
	t.Helper()
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-sd2-ledger", 8000)
	input := taskSubmissionTestInput(userID, tokenID, requestID, 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)
	confirmed, err := CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "provider_task_ledger", &model.Task{
		Properties: model.Properties{
			OriginModelName:   "verdantflare-sd2",
			UpstreamModelName: "doubao-seedance-2.0",
		},
	}, "system")
	require.NoError(t, err)
	require.NotNil(t, confirmed.TaskDBID)
	var task model.Task
	require.NoError(t, model.DB.First(&task, *confirmed.TaskDBID).Error)
	return confirmed, &task
}

func configureModelSD2LedgerStore(t *testing.T) {
	t.Helper()
	SetSD2LedgerPollingStore(modelSD2LedgerPollingStore{})
	SetSD2ResultHostAllowlistResolver(func(*model.TaskSubmission) []string {
		return []string{"result.example"}
	})
	t.Cleanup(func() {
		SetSD2LedgerPollingStore(modelSD2LedgerPollingStore{})
		SetSD2ResultHostAllowlistResolver(nil)
		SetSD2ResultArchive(nil)
	})
}

func bindSD2LedgerPollingChannel(t *testing.T, submissionID int64, task *model.Task) (*model.Channel, string) {
	t.Helper()
	t.Setenv(SD2RequestIdentityHMACKeyEnv, "0123456789abcdef0123456789abcdef")
	channelKey := "secret-never-logged"
	require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Where("id = ?", submissionID).Update("credential_fingerprint", SD2ChannelCredentialFingerprint(channelKey)).Error)
	baseURL := "https://wxmaas.clarmic.com"
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, task.ChannelId).Error)
	channel.Type = constant.ChannelTypeWxmaasSeedance
	channel.Key = channelKey
	channel.BaseURL = &baseURL
	return &channel, task.GetUpstreamTaskID()
}

func TestSD2LedgerPollingUsesIndependentChannelSwitch(t *testing.T) {
	t.Run("create disabled keeps confirmed polling active", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 611, 611, "622902c8-fc08-4be2-bf09-eb0dc84d21b0")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		setting := `{"create_enabled":false,"poll_enabled":true,"max_concurrency":1,"max_task_cost_microunits_cny":1000000,"hard_daily_budget_microunits_cny":100000000}`
		channel.Setting = &setting
		adaptor := &fixedSD2PollingAdaptor{
			statusCode: http.StatusOK,
			body:       []byte(`{"provider":"safe-shape"}`),
			taskResult: &relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusInProgress},
		}

		require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task}))
		assert.Equal(t, 1, adaptor.fetchCalls)
	})

	t.Run("poll disabled pauses without provider request", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 612, 612, "57d1ad14-5417-4f92-9d86-e6f8ac044dd9")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		setting := `{"create_enabled":true,"poll_enabled":false,"max_concurrency":1,"max_task_cost_microunits_cny":1000000,"hard_daily_budget_microunits_cny":100000000}`
		channel.Setting = &setting
		adaptor := &fixedSD2PollingAdaptor{statusCode: http.StatusOK}

		require.ErrorContains(t, updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task}), "poll safety gate rejected")
		assert.Zero(t, adaptor.fetchCalls)
		var reloaded model.TaskSubmission
		require.NoError(t, model.DB.First(&reloaded, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionPollStatePausedCredential, reloaded.PollState)
		assert.Equal(t, "channel_poll_disabled", reloaded.SafeErrorCode)
	})
}

func TestSD2LedgerProviderSuccessRequiresPrivateArchiveBeforeCompletion(t *testing.T) {
	truncate(t)
	configureModelSD2LedgerStore(t)
	disableSSRFForSD2ArchiveTest(t)
	confirmed, task := seedConfirmedSD2LedgerTask(t, 601, 601, "32f4313b-c98d-4423-b42b-a416dcb95cf8")
	link, linked, err := lookupSD2LedgerTask(context.Background(), task.ID)
	require.NoError(t, err)
	require.True(t, linked)

	SetSD2ResultArchive(nil)
	err = handleSD2LedgerTaskResult(context.Background(), task, link, &relaycommon.TaskInfo{
		Status:           model.TaskStatusSuccess,
		Url:              "https://result.example/video.mp4?signature=secret",
		CompletionTokens: 50,
		TotalTokens:      100,
	})
	require.EqualError(t, err, "sd2 result archival failed")

	var reloadedTask model.Task
	require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
	assert.NotEqual(t, model.TaskStatusSuccess, reloadedTask.Status)
	assert.Empty(t, reloadedTask.PrivateData.ResultURL)
	var reloadedSubmission model.TaskSubmission
	require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
	assert.Equal(t, model.TaskSubmissionArchiveStateFailed, reloadedSubmission.ArchiveState)
	assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
	assert.Equal(t, model.TaskSubmissionPollStateBackoff, reloadedSubmission.PollState)
	assert.Equal(t, model.TaskSubmissionProviderCostRecorded, reloadedSubmission.ProviderCostState)
	require.NotNil(t, reloadedSubmission.ProviderCostMicrounits)
	assert.Equal(t, int64(4600), *reloadedSubmission.ProviderCostMicrounits)
	assert.Equal(t, 9000, getUserQuota(t, task.UserId))
}

func TestSD2LedgerArchivedSuccessCompletesAndSettlesAtomically(t *testing.T) {
	truncate(t)
	configureModelSD2LedgerStore(t)
	disableSSRFForSD2ArchiveTest(t)
	confirmed, task := seedConfirmedSD2LedgerTask(t, 602, 602, "a565c87e-ee37-472c-b8e4-1cb31c6573d3")
	link, linked, err := lookupSD2LedgerTask(context.Background(), task.ID)
	require.NoError(t, err)
	require.True(t, linked)
	archivedResult := validTestSD2ArchivedResult("sd2-result://private-results/user-602/task.mp4", 2048)
	SetSD2ResultArchive(&fakeSD2ResultArchive{archiveResult: archivedResult})
	seed := int64(-1)
	generateAudio := false

	err = handleSD2LedgerTaskResult(context.Background(), task, link, &relaycommon.TaskInfo{
		Status:            model.TaskStatusSuccess,
		Url:               "https://result.example/video.mp4?signature=secret",
		CompletionTokens:  50,
		TotalTokens:       100,
		Seed:              &seed,
		Resolution:        "720p",
		Duration:          15,
		Ratio:             "16:9",
		FramesPerSecond:   24,
		GenerateAudio:     &generateAudio,
		ProviderCreatedAt: 1785916800,
		ProviderUpdatedAt: 1785916815,
	})
	require.NoError(t, err)

	var reloadedTask model.Task
	require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), reloadedTask.Status)
	assert.Contains(t, reloadedTask.PrivateData.ResultURL, "/v1/videos/"+task.TaskID+"/content")
	snapshot, err := DecodeSD2TaskResultSnapshot(reloadedTask.Data)
	require.NoError(t, err)
	assert.Equal(t, SD2ArchiveStateArchived, snapshot.ArchiveState)
	require.NotNil(t, snapshot.ArchivedResult)
	assert.Equal(t, archivedResult, *snapshot.ArchivedResult)
	require.NotNil(t, snapshot.Seed)
	assert.Equal(t, int64(-1), *snapshot.Seed)
	assert.Equal(t, "720p", snapshot.Resolution)
	assert.Equal(t, 15, snapshot.Duration)
	assert.Equal(t, "16:9", snapshot.Ratio)
	assert.Equal(t, 24, snapshot.FramesPerSecond)
	require.NotNil(t, snapshot.GenerateAudio)
	assert.False(t, *snapshot.GenerateAudio)

	var reloadedSubmission model.TaskSubmission
	require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
	assert.Equal(t, model.TaskSubmissionArchiveStateArchived, reloadedSubmission.ArchiveState)
	assert.Equal(t, model.TaskSubmissionBillingStateSettled, reloadedSubmission.BillingState)
	assert.Equal(t, model.TaskSubmissionPollStateTerminal, reloadedSubmission.PollState)
	frozenDescriptor, err := SD2ArchivedResultFromSubmission(&reloadedSubmission)
	require.NoError(t, err)
	assert.Equal(t, archivedResult, frozenDescriptor)
	assert.Equal(t, model.TaskSubmissionProviderCostRecorded, reloadedSubmission.ProviderCostState)
	require.NotNil(t, reloadedSubmission.ProviderCostMicrounits)
	assert.Equal(t, int64(4600), *reloadedSubmission.ProviderCostMicrounits)
	assert.Equal(t, 9000, getUserQuota(t, task.UserId))
}

func TestSD2LedgerProviderFailureRefundsOnceButExpiredDoesNotRefund(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 603, 603, "6d575434-191e-4d87-bc36-adc242d04d0f")
		link, linked, err := lookupSD2LedgerTask(context.Background(), task.ID)
		require.NoError(t, err)
		require.True(t, linked)

		err = handleSD2LedgerTaskResult(context.Background(), task, link, &relaycommon.TaskInfo{
			Status:      model.TaskStatusFailure,
			Reason:      "provider_failed",
			TotalTokens: 100,
		})
		require.NoError(t, err)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionBillingStateRefunded, reloadedSubmission.BillingState)
		assert.Equal(t, model.TaskSubmissionPollStateTerminal, reloadedSubmission.PollState)
		assert.Equal(t, model.TaskSubmissionProviderCostRecorded, reloadedSubmission.ProviderCostState)
		require.NotNil(t, reloadedSubmission.ProviderCostMicrounits)
		assert.Equal(t, int64(4600), *reloadedSubmission.ProviderCostMicrounits)
		assert.Equal(t, 10000, getUserQuota(t, task.UserId))
		assert.Equal(t, 8000, getTokenRemainQuota(t, 603))
	})

	t.Run("expired", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 604, 604, "90dc4d02-a742-44b6-8cbe-d3b79da20331")
		link, linked, err := lookupSD2LedgerTask(context.Background(), task.ID)
		require.NoError(t, err)
		require.True(t, linked)

		err = handleSD2LedgerTaskResult(context.Background(), task, link, &relaycommon.TaskInfo{
			Status: model.TaskStatusUnknown,
			Reason: "provider_expired",
		})
		require.NoError(t, err)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		assert.Equal(t, "provider_expired", reloadedTask.FailReason)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
		assert.Equal(t, model.TaskSubmissionProviderCostReconcileRequired, reloadedSubmission.ProviderCostState)
		assert.Equal(t, model.TaskSubmissionPollStateTerminal, reloadedSubmission.PollState)
		assert.Equal(t, 9000, getUserQuota(t, task.UserId))
		assert.Equal(t, 7000, getTokenRemainQuota(t, 604))
	})

	t.Run("cancelled failure-shaped response remains unresolved", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 610, 610, "112a3f85-c51f-4784-a886-59ae6c3c98ce")
		link, linked, err := lookupSD2LedgerTask(context.Background(), task.ID)
		require.NoError(t, err)
		require.True(t, linked)

		err = handleSD2LedgerTaskResult(context.Background(), task, link, &relaycommon.TaskInfo{
			Status: model.TaskStatusFailure,
			Reason: "provider_cancelled",
		})
		require.NoError(t, err)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		assert.Equal(t, "provider_cancelled", reloadedTask.FailReason)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
		assert.Equal(t, model.TaskSubmissionProviderCostReconcileRequired, reloadedSubmission.ProviderCostState)
		assert.Equal(t, model.TaskSubmissionPollStateTerminal, reloadedSubmission.PollState)
		assert.Equal(t, "provider_cancelled", reloadedSubmission.SafeErrorCode)
		assert.Equal(t, 9000, getUserQuota(t, task.UserId))
	})
}

func TestSD2LedgerPollingInfrastructureErrorsDoNotFailOrRefundTask(t *testing.T) {
	t.Run("provider auth response", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 605, 605, "a1f8df54-50a5-4730-a783-965a364b78be")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		err := updateVideoSingleTask(context.Background(), &fixedSD2PollingAdaptor{statusCode: http.StatusUnauthorized}, channel, upstreamID, map[string]*model.Task{upstreamID: task})
		require.Error(t, err)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.NotEqual(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionPollStatePausedCredential, reloadedSubmission.PollState)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
		assert.Equal(t, 9000, getUserQuota(t, task.UserId))
	})

	t.Run("local timeout", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 606, 606, "62ee2f4f-b421-4895-97cb-297a9d99f04f")
		oldSubmitTime := time.Now().Add(-2 * time.Hour).Unix()
		require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Updates(map[string]any{
			"submit_time": oldSubmitTime,
			"progress":    "10%",
		}).Error)
		previousTimeout := constant.TaskTimeoutMinutes
		constant.TaskTimeoutMinutes = 1
		t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

		sweepTimedOutTasks(context.Background())

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.NotEqual(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionPollStateStale, reloadedSubmission.PollState)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
		assert.Equal(t, model.TaskSubmissionProviderCostReconcileRequired, reloadedSubmission.ProviderCostState)
		assert.Equal(t, 9000, getUserQuota(t, task.UserId))
	})
}

func TestSD2LedgerQueryUsesBoundedAdaptorParsingAndTaskBinding(t *testing.T) {
	t.Run("oversized provider body is rejected before parsing", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 607, 607, "1d05b778-22ec-465f-971c-e17b2922ed05")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		adaptor := &fixedSD2PollingAdaptor{
			statusCode: http.StatusOK,
			body:       bytes.Repeat([]byte("x"), int(maxSD2LedgerPollResponseBytes+1)),
			taskResult: &relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusSuccess},
		}

		err := updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task})
		require.ErrorContains(t, err, "response too large")
		assert.Zero(t, adaptor.parseCalls)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.NotEqual(t, model.TaskStatus(model.TaskStatusSuccess), reloadedTask.Status)
		assert.NotEqual(t, model.TaskStatus(model.TaskStatusFailure), reloadedTask.Status)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionPollStateBackoff, reloadedSubmission.PollState)
		assert.Equal(t, "provider_query_response_too_large", reloadedSubmission.SafeErrorCode)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
	})

	t.Run("legacy JD provider body is also bounded before parsing", func(t *testing.T) {
		truncate(t)
		const upstreamID = "provider_task_private"
		task := &model.Task{
			ID:        990001,
			TaskID:    "task_public_safe",
			ChannelId: 990001,
			PrivateData: model.TaskPrivateData{
				UpstreamTaskID: upstreamID,
			},
		}
		channel := &model.Channel{Id: task.ChannelId, Type: constant.ChannelTypeJDSeedance, Key: "provider-key"}
		adaptor := &fixedSD2PollingAdaptor{
			statusCode: http.StatusOK,
			body:       bytes.Repeat([]byte("x"), int(maxSD2LedgerPollResponseBytes+1)),
			taskResult: &relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusSuccess},
		}

		err := updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task})
		require.ErrorContains(t, err, "response too large")
		assert.Zero(t, adaptor.parseCalls)
	})

	t.Run("generic task envelope cannot bypass provider adaptor", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 608, 608, "62fd6157-0491-47b4-af73-e66ac06ef288")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		adaptor := &fixedSD2PollingAdaptor{
			statusCode: http.StatusOK,
			body:       []byte(`{"code":"success","data":{"task_id":"untrusted-envelope-task","status":"SUCCESS"}}`),
			taskResult: &relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusInProgress, Progress: "50%"},
		}
		task.PrivateData.Key = "stale-task-private-key"

		require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task}))
		assert.Equal(t, 1, adaptor.parseCalls)
		assert.Equal(t, channel.Key, adaptor.fetchedKey)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), reloadedTask.Status)
		assert.NotContains(t, string(reloadedTask.Data), "untrusted-envelope-task")
	})

	t.Run("provider task id mismatch is never applied", func(t *testing.T) {
		truncate(t)
		configureModelSD2LedgerStore(t)
		confirmed, task := seedConfirmedSD2LedgerTask(t, 609, 609, "4b9ea094-b6c7-4868-a815-03091513399f")
		channel, upstreamID := bindSD2LedgerPollingChannel(t, confirmed.ID, task)
		adaptor := &fixedSD2PollingAdaptor{
			statusCode: http.StatusOK,
			body:       []byte(`{"provider":"safe-shape"}`),
			taskResult: &relaycommon.TaskInfo{TaskID: "another-provider-task", Status: model.TaskStatusSuccess, Url: "https://result.example/wrong.mp4"},
		}

		err := updateVideoSingleTask(context.Background(), adaptor, channel, upstreamID, map[string]*model.Task{upstreamID: task})
		require.ErrorContains(t, err, "task binding mismatch")
		assert.Equal(t, 1, adaptor.parseCalls)

		var reloadedTask model.Task
		require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
		assert.NotEqual(t, model.TaskStatus(model.TaskStatusSuccess), reloadedTask.Status)
		assert.Empty(t, reloadedTask.PrivateData.ResultURL)
		var reloadedSubmission model.TaskSubmission
		require.NoError(t, model.DB.First(&reloadedSubmission, confirmed.ID).Error)
		assert.Equal(t, model.TaskSubmissionPollStateBackoff, reloadedSubmission.PollState)
		assert.Equal(t, "provider_task_id_mismatch", reloadedSubmission.SafeErrorCode)
		assert.Equal(t, model.TaskSubmissionBillingStateCommitted, reloadedSubmission.BillingState)
	})
}
