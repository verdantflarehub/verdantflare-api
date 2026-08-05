package service

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setWxmaasSafetySetting(t *testing.T, createEnabled, pollEnabled bool, maxTaskCost, dailyBudget int64) {
	t.Helper()
	setting := `{"create_enabled":` + boolString(createEnabled) + `,"poll_enabled":` + boolString(pollEnabled) + `,"max_concurrency":1,"max_task_cost_microunits_cny":` + int64String(maxTaskCost) + `,"hard_daily_budget_microunits_cny":` + int64String(dailyBudget) + `}`
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 91).Update("setting", setting).Error)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func int64String(value int64) string {
	return fmt.Sprintf("%d", value)
}

func TestWxmaasSafetyGateRejectsMissingConfiguration(t *testing.T) {
	truncate(t)
	seedUser(t, 901, 10000)
	seedToken(t, 901, 901, "sk-safety-missing", 10000)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 91).Update("setting", `{}`).Error)

	_, created, err := PrepareTaskSubmission(taskSubmissionTestInput(901, 901, "29392934-dd70-41c8-aa00-591f99fb056f", 1000))
	assert.False(t, created)
	assert.ErrorIs(t, err, ErrSD2ChannelSafetyConfig)
	assert.Equal(t, 10000, getUserQuota(t, 901))
	var count int64
	require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestWxmaasSafetyGateSeparatesCreateAndPollSwitches(t *testing.T) {
	truncate(t)
	seedUser(t, 902, 10000)
	seedToken(t, 902, 902, "sk-safety-switch", 10000)
	setWxmaasSafetySetting(t, false, true, 1000, 10000)

	_, created, err := PrepareTaskSubmission(taskSubmissionTestInput(902, 902, "05b58ca0-714b-4b0f-a9a3-0ddc8bb84b49", 1000))
	assert.False(t, created)
	assert.ErrorIs(t, err, ErrSD2ChannelCreateDisabled)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, 91).Error)
	pollAllowed, err := WxmaasChannelPollingAllowed(&channel)
	require.NoError(t, err)
	assert.True(t, pollAllowed)
}

func TestWxmaasSafetyGateT2RecheckRejectsAndRefunds(t *testing.T) {
	truncate(t)
	seedUser(t, 903, 10000)
	seedToken(t, 903, 903, "sk-safety-t2", 10000)
	setWxmaasSafetySetting(t, true, true, 1000, 10000)

	submission, created, err := PrepareTaskSubmission(taskSubmissionTestInput(903, 903, "1ea4abef-72c6-42f7-9a30-b137db0f48e9", 1000))
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 9000, getUserQuota(t, 903))
	setWxmaasSafetySetting(t, false, true, 1000, 10000)

	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	assert.Nil(t, claimed)
	assert.False(t, won)
	assert.ErrorIs(t, err, ErrSD2ChannelCreateDisabled)
	assert.Equal(t, 10000, getUserQuota(t, 903))
	reloaded, err := model.GetTaskSubmissionByID(submission.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionStateRejected, reloaded.State)
	assert.Equal(t, model.TaskSubmissionBillingStateRefunded, reloaded.BillingState)
}

func TestWxmaasSafetyGateCountsPotentialAndRecordedDailyCost(t *testing.T) {
	truncate(t)
	now := time.Now().Unix()
	states := []model.TaskSubmissionState{
		model.TaskSubmissionStateSending,
		model.TaskSubmissionStateUnknown,
		model.TaskSubmissionStateConfirmed,
	}
	for index, state := range states {
		require.NoError(t, model.DB.Create(&model.TaskSubmission{
			UserID: 910 + index, ClientRequestID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), DigestVersion: "v1",
			BillingRequestID: "exposure:" + uuid.NewString(),
			PublicTaskID:     model.GenerateTaskID(), State: state, ChannelID: 91, ChannelType: 60,
			ProviderCostState: model.TaskSubmissionProviderCostPending, ProviderCostPotentialMicrounits: 200,
			CreatedAt: now, UpdatedAt: now, BillingState: model.TaskSubmissionBillingStateReserved,
		}).Error)
	}
	recorded := int64(350)
	require.NoError(t, model.DB.Create(&model.TaskSubmission{
		UserID: 920, ClientRequestID: uuid.NewString(), RequestDigest: strings.Repeat("b", 64), DigestVersion: "v1",
		BillingRequestID: "exposure:" + uuid.NewString(),
		PublicTaskID:     model.GenerateTaskID(), State: model.TaskSubmissionStateConfirmed, ChannelID: 91, ChannelType: 60,
		ProviderCostState: model.TaskSubmissionProviderCostRecorded, ProviderCostMicrounits: &recorded,
		CreatedAt: now, UpdatedAt: now, BillingState: model.TaskSubmissionBillingStateSettled,
	}).Error)
	require.NoError(t, model.DB.Create(&model.TaskSubmission{
		UserID: 921, ClientRequestID: uuid.NewString(), RequestDigest: strings.Repeat("c", 64), DigestVersion: "v1",
		BillingRequestID: "exposure:" + uuid.NewString(),
		PublicTaskID:     model.GenerateTaskID(), State: model.TaskSubmissionStateConfirmed, ChannelID: 91, ChannelType: 60,
		ProviderCostState: model.TaskSubmissionProviderCostRecorded, ProviderCostPotentialMicrounits: 150,
		CreatedAt: now, UpdatedAt: now, BillingState: model.TaskSubmissionBillingStateSettled,
	}).Error)

	exposure, err := wxmaasDailyExposureTx(model.DB, 91, time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(3), exposure.ActiveCount)
	assert.Equal(t, int64(600), exposure.PotentialCost)
	assert.Equal(t, int64(500), exposure.RecordedCost)
}

func TestWxmaasSafetyGateRejectsDailyBudget(t *testing.T) {
	truncate(t)
	seedUser(t, 904, 10000)
	seedToken(t, 904, 904, "sk-safety-budget", 10000)
	setWxmaasSafetySetting(t, true, true, 600, 1000)
	recorded := int64(600)
	now := time.Now().Unix()
	require.NoError(t, model.DB.Create(&model.TaskSubmission{
		UserID: 999, ClientRequestID: "f998bce9-2410-4ef4-b602-edfa61b16f33", RequestDigest: strings.Repeat("d", 64), DigestVersion: "v1",
		BillingRequestID: "exposure:" + uuid.NewString(),
		PublicTaskID:     model.GenerateTaskID(), State: model.TaskSubmissionStateConfirmed, ChannelID: 91, ChannelType: 60,
		ProviderCostState: model.TaskSubmissionProviderCostRecorded, ProviderCostMicrounits: &recorded,
		CreatedAt: now, UpdatedAt: now, BillingState: model.TaskSubmissionBillingStateSettled,
	}).Error)

	_, created, err := PrepareTaskSubmission(taskSubmissionTestInput(904, 904, "78ea63da-6134-4447-9afa-6f620cdca4f8", 1000))
	assert.False(t, created)
	assert.ErrorIs(t, err, ErrSD2ChannelDailyBudget)
}

func TestWxmaasSafetyGateConcurrencyDoesNotResetAtMidnight(t *testing.T) {
	truncate(t)
	seedUser(t, 906, 10000)
	seedToken(t, 906, 906, "sk-safety-prior-day", 10000)
	setWxmaasSafetySetting(t, true, true, 1000, 10000)
	priorDay := time.Now().UTC().Add(-24 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(&model.TaskSubmission{
		UserID: 998, ClientRequestID: uuid.NewString(), RequestDigest: strings.Repeat("e", 64), DigestVersion: "v1",
		BillingRequestID: "exposure:" + uuid.NewString(),
		PublicTaskID:     model.GenerateTaskID(), State: model.TaskSubmissionStateUnknown, ChannelID: 91, ChannelType: 60,
		ProviderCostState: model.TaskSubmissionProviderCostPending, ProviderCostPotentialMicrounits: 1000,
		CreatedAt: priorDay, UpdatedAt: priorDay, BillingState: model.TaskSubmissionBillingStateReserved,
	}).Error)

	_, created, err := PrepareTaskSubmission(taskSubmissionTestInput(906, 906, "5e17c811-fe55-40b6-9fd2-d615dc9a0771", 1000))
	assert.False(t, created)
	assert.ErrorIs(t, err, ErrSD2ChannelConcurrencyLimit)
}

func TestWxmaasSafetyGateSerializesConcurrentSQLiteCreates(t *testing.T) {
	truncate(t)
	seedUser(t, 905, 10000)
	seedToken(t, 905, 905, "sk-safety-concurrent", 10000)
	setWxmaasSafetySetting(t, true, true, 1000, 10000)

	inputs := []PrepareTaskSubmissionInput{
		taskSubmissionTestInput(905, 905, "d3d52b3f-0704-4165-b79b-9d2974e3dd36", 1000),
		taskSubmissionTestInput(905, 905, "285c7c1b-cc02-450e-8c07-2ec793be4a92", 1000),
	}
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, len(inputs))
	var wait sync.WaitGroup
	for _, input := range inputs {
		wait.Add(1)
		go func(value PrepareTaskSubmissionInput) {
			defer wait.Done()
			_, created, err := PrepareTaskSubmission(value)
			results <- result{created: created, err: err}
		}(input)
	}
	wait.Wait()
	close(results)

	createdCount := 0
	rejectedCount := 0
	for result := range results {
		if result.created && result.err == nil {
			createdCount++
		}
		if errors.Is(result.err, ErrSD2ChannelConcurrencyLimit) {
			rejectedCount++
		}
	}
	assert.Equal(t, 1, createdCount)
	assert.Equal(t, 1, rejectedCount)
	var count int64
	require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
