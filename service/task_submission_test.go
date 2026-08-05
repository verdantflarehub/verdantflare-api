package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func taskSubmissionTestInput(userID int, tokenID int, requestID string, quota int) PrepareTaskSubmissionInput {
	return PrepareTaskSubmissionInput{
		UserID:                userID,
		ClientRequestID:       requestID,
		RequestDigest:         strings.Repeat("a", 64),
		DigestVersion:         "hmac-sha256-v1",
		ProviderRequestDigest: strings.Repeat("b", 64),
		ChannelID:             91,
		ChannelType:           60,
		CanonicalBaseOrigin:   "https://wxmaas.clarmic.com/",
		Provider:              "wxmaas-seedance",
		LogicalAccountID:      "wx-account-1",
		CredentialRef:         "secret/wx-account-1/version/1",
		CredentialFingerprint: strings.Repeat("c", 64),
		OriginModelName:       "verdantflare-sd2",
		UpstreamModelName:     "doubao-seedance-2.0",
		ReservedQuota:         quota,
		TokenID:               tokenID,
		FundingSource:         BillingSourceWallet,
		RequestSummary: SubmissionRequestSummary{
			SchemaVersion: 1,
			Action:        "create",
			Duration:      15,
			Ratio:         "16:9",
			Resolution:    "720p",
			ImageCount:    1,
			VideoCount:    0,
			AudioCount:    0,
			GenerateAudio: true,
			Watermark:     false,
		},
		PublicPriceSnapshot: SubmissionPublicPriceSnapshot{
			SchemaVersion: 1,
			PriceVersion:  "verdantflare-sd2-fixed-v1",
			ModelName:     "verdantflare-sd2",
			ProductQuota:  quota,
			GroupRatio:    "1.0",
			Currency:      "USD",
		},
		ProviderCostSnapshot: SubmissionProviderCostSnapshot{
			SchemaVersion:          1,
			ProviderPriceKey:       "wxmaas/doubao-seedance-2.0/2026-08-05",
			ProviderPriceVersion:   "2026-08-05",
			Provider:               "wxmaas-seedance",
			UpstreamModelName:      "doubao-seedance-2.0",
			Resolution:             "720p",
			HasVideo:               false,
			Duration:               15,
			ImageCount:             1,
			VideoCount:             0,
			AudioCount:             0,
			UnitPricePerMillionCNY: "46",
			RoundingRule:           "ROUND_HALF_UP",
		},
	}
}

func TestPrepareTaskSubmissionReservesExactlyOnce(t *testing.T) {
	truncate(t)
	const userID, tokenID, quota = 501, 501, 2000
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-wallet", 8000)

	input := taskSubmissionTestInput(userID, tokenID, "60bdc8ee-2fd7-4d72-9395-96ae0d05df80", quota)
	submission, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, model.TaskSubmissionStatePrepared, submission.State)
	assert.Equal(t, model.TaskSubmissionBillingStateReserved, submission.BillingState)
	assert.Equal(t, int64(2), submission.Version)
	assert.Equal(t, quota, submission.FundingReservedQuota)
	assert.Equal(t, quota, submission.TokenReservedQuota)
	assert.Equal(t, 8000, getUserQuota(t, userID))
	assert.Equal(t, 6000, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, quota, getTokenUsedQuota(t, tokenID))

	replayed, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, submission.ID, replayed.ID)
	assert.Equal(t, 8000, getUserQuota(t, userID))
	assert.Equal(t, 6000, getTokenRemainQuota(t, tokenID))

	var entries int64
	require.NoError(t, model.DB.Model(&model.TaskSubmissionBillingEntry{}).Where("submission_id = ?", submission.ID).Count(&entries).Error)
	assert.Equal(t, int64(1), entries)
	var outbox int64
	require.NoError(t, model.DB.Model(&model.QuotaCacheInvalidationOutbox{}).Count(&outbox).Error)
	assert.Equal(t, int64(1), outbox)
}

func TestPrepareTaskSubmissionRejectsDigestConflictWithoutCharging(t *testing.T) {
	truncate(t)
	const userID, tokenID = 502, 502
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-conflict", 8000)

	input := taskSubmissionTestInput(userID, tokenID, "c16a286a-3a48-48d6-8cba-394b790b59a2", 1000)
	_, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	require.True(t, created)

	input.RequestDigest = strings.Repeat("d", 64)
	_, created, err = PrepareTaskSubmission(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionIdempotencyConflict)
	assert.False(t, created)
	assert.Equal(t, 9000, getUserQuota(t, userID))
	assert.Equal(t, 7000, getTokenRemainQuota(t, tokenID))
}

func TestPrepareTaskSubmissionRollsBackPartialReservation(t *testing.T) {
	truncate(t)
	const userID, tokenID = 503, 503
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-insufficient-token", 500)

	input := taskSubmissionTestInput(userID, tokenID, "b20ed22d-eae2-4db1-afdb-3880f83ec555", 1000)
	_, created, err := PrepareTaskSubmission(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionInsufficientQuota)
	assert.False(t, created)
	assert.Equal(t, 10000, getUserQuota(t, userID))
	assert.Equal(t, 500, getTokenRemainQuota(t, tokenID))

	var submissions int64
	require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Count(&submissions).Error)
	assert.Zero(t, submissions)
	var entries int64
	require.NoError(t, model.DB.Model(&model.TaskSubmissionBillingEntry{}).Count(&entries).Error)
	assert.Zero(t, entries)
}

func TestAcquireSubmissionSendHasOneWinnerAndUnknownDoesNotRefund(t *testing.T) {
	truncate(t)
	const userID, tokenID = 504, 504
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-send", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "11f30c66-73d9-4d0d-bcf4-744138fa12d6", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)

	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, model.TaskSubmissionStateSending, claimed.State)
	assert.Equal(t, 1, claimed.SendAttempts)
	assert.Equal(t, model.TaskSubmissionProviderCostPending, claimed.ProviderCostState)

	_, won, err = AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	assert.False(t, won)

	unknown, err := MarkSubmissionUnknown(claimed.ID, claimed.Version, "provider_response_unknown", "Authorization: Bearer secret at https://signed.example/video", nil, "")
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionStateUnknown, unknown.State)
	assert.Equal(t, model.TaskSubmissionBillingStateReserved, unknown.BillingState)
	assert.NotContains(t, unknown.SafeErrorMessage, "secret")
	assert.NotContains(t, unknown.SafeErrorMessage, "https://")
	assert.Equal(t, 9000, getUserQuota(t, userID))
	assert.Equal(t, 7000, getTokenRemainQuota(t, tokenID))

	replayed, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, model.TaskSubmissionStateUnknown, replayed.State)
}

func TestCommitSubmissionPersistsTaskBeforeConfirmation(t *testing.T) {
	truncate(t)
	const userID, tokenID = 505, 505
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-commit", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "5cc56c4e-8f56-440d-9621-280965b2ad61", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)

	task := &model.Task{Properties: model.Properties{OriginModelName: "verdantflare-sd2", UpstreamModelName: "doubao-seedance-2.0"}}
	task.SetData(map[string]any{"schema_version": 1})
	confirmed, err := CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "provider_task_123", task, "system")
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionStateConfirmed, confirmed.State)
	assert.Equal(t, model.TaskSubmissionBillingStateCommitted, confirmed.BillingState)
	require.NotNil(t, confirmed.TaskDBID)
	require.NotNil(t, confirmed.UpstreamTaskID)
	assert.Equal(t, "provider_task_123", *confirmed.UpstreamTaskID)

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, *confirmed.TaskDBID).Error)
	assert.Equal(t, confirmed.PublicTaskID, persisted.TaskID)
	assert.Equal(t, "provider_task_123", persisted.PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), persisted.Status)
	assert.Equal(t, 1000, persisted.Quota)

	// A DB-only retry with the same provider result is an idempotent read, not a
	// second Task insert.
	replayed, err := CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "provider_task_123", &model.Task{}, "system")
	require.NoError(t, err)
	assert.Equal(t, confirmed.ID, replayed.ID)
	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Where("task_id = ?", confirmed.PublicTaskID).Count(&taskCount).Error)
	assert.Equal(t, int64(1), taskCount)
}

func TestRejectSubmissionRefundsFrozenWalletAndTokenOnce(t *testing.T) {
	truncate(t)
	const userID, tokenID = 506, 506
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-reject", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "d18bb81e-b0a1-43bb-b0ea-83cf468b9bc4", 1500)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)

	var rejected *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		rejected, inner = RejectSubmissionTx(tx, claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "documented_not_created", "admin:1")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionStateRejected, rejected.State)
	assert.Equal(t, model.TaskSubmissionBillingStateRefunded, rejected.BillingState)
	assert.Equal(t, 10000, getUserQuota(t, userID))
	assert.Equal(t, 8000, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		_, inner := RejectSubmissionTx(tx, claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "documented_not_created", "admin:1")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, 10000, getUserQuota(t, userID))
	assert.Equal(t, 8000, getTokenRemainQuota(t, tokenID))
	var refundCount int64
	require.NoError(t, model.DB.Model(&model.TaskSubmissionBillingEntry{}).Where("submission_id = ? AND operation = ?", claimed.ID, model.TaskSubmissionBillingOperationRefund).Count(&refundCount).Error)
	assert.Equal(t, int64(1), refundCount)
}

func TestRefundRestoresOnlyQuotaDerivedExhaustedTokenState(t *testing.T) {
	truncate(t)
	const userID, tokenID, quota = 516, 516, 1000
	seedUser(t, userID, quota)
	seedToken(t, tokenID, userID, "sk-submission-exact-balance", quota)

	input := taskSubmissionTestInput(userID, tokenID, "4086be0d-cfe2-43e6-9545-399796d8578b", quota)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	require.Zero(t, getTokenRemainQuota(t, tokenID))
	require.NoError(t, model.DB.Unscoped().Model(&model.Token{}).Where("id = ?", tokenID).Update("status", common.TokenStatusExhausted).Error)

	var refunded *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		refunded, inner = RejectSubmissionTx(tx, submission.ID, submission.Version, model.TaskSubmissionStatePrepared, "verified_not_created", "system")
		return inner
	})
	require.NoError(t, err)
	require.Equal(t, model.TaskSubmissionBillingStateRefunded, refunded.BillingState)
	require.Equal(t, quota, getTokenRemainQuota(t, tokenID))

	var token model.Token
	require.NoError(t, model.DB.Unscoped().First(&token, tokenID).Error)
	require.Equal(t, common.TokenStatusEnabled, token.Status)
}

func TestSubscriptionReservationAndRefundFreezeExactAmounts(t *testing.T) {
	truncate(t)
	const userID, tokenID, subscriptionID = 507, 507, 507
	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, "sk-submission-subscription", 8000)
	seedSubscription(t, subscriptionID, userID, 10000, 2000)
	input := taskSubmissionTestInput(userID, tokenID, "10230b9a-1954-448f-8813-91c2f131f771", 1500)
	input.FundingSource = BillingSourceSubscription
	input.SubscriptionID = subscriptionID

	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	require.NotNil(t, submission.SubscriptionID)
	assert.Equal(t, subscriptionID, *submission.SubscriptionID)
	assert.Equal(t, int64(3500), getSubscriptionUsed(t, subscriptionID))
	assert.Equal(t, 6500, getTokenRemainQuota(t, tokenID))

	var refunded *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		refunded, inner = RejectSubmissionTx(tx, submission.ID, submission.Version, model.TaskSubmissionStatePrepared, "local_expiry_before_send", "system")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionBillingStateRefunded, refunded.BillingState)
	assert.Equal(t, int64(2000), getSubscriptionUsed(t, subscriptionID))
	assert.Equal(t, 8000, getTokenRemainQuota(t, tokenID))
}

func TestQuotaCacheInvalidationOutboxCompletesAfterCommit(t *testing.T) {
	truncate(t)
	const userID, tokenID = 508, 508
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-outbox", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "943505cb-19ee-4cb0-a58b-ae0301a7e10a", 1000)
	_, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)

	processed, err := ProcessQuotaCacheInvalidationOutbox(t.Context(), "test-worker", 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	var event model.QuotaCacheInvalidationOutbox
	require.NoError(t, model.DB.First(&event).Error)
	require.NotNil(t, event.ProcessedAt)
	assert.Empty(t, event.LockedBy)
	assert.Greater(t, event.BalanceVersion, int64(0))
}

func TestArchiveRequiredBeforeSettlement(t *testing.T) {
	truncate(t)
	const userID, tokenID = 509, 509
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-archive", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "c666f2d6-cedf-41e8-aa93-518c25c6af89", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)
	confirmed, err := CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "provider_task_archive", &model.Task{}, "system")
	require.NoError(t, err)

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		_, inner := SettleSubmissionTx(tx, confirmed.ID, confirmed.Version, "provider_succeeded", "system")
		return inner
	})
	assert.ErrorIs(t, err, ErrTaskSubmissionCASLost)

	var settled *model.TaskSubmission
	archivedResult := validTestSD2ArchivedResult("sd2-result://private-results/2026/08/task.mp4", 1024)
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		settled, inner = ArchiveAndSettleSubmissionTx(tx, confirmed.ID, confirmed.Version, archivedResult, "system")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionArchiveStateArchived, settled.ArchiveState)
	assert.Equal(t, model.TaskSubmissionBillingStateSettled, settled.BillingState)
	assert.Equal(t, model.TaskSubmissionPollStateTerminal, settled.PollState)
	assert.Equal(t, model.TaskSubmissionProviderCostReconcileRequired, settled.ProviderCostState)
	frozenDescriptor, err := SD2ArchivedResultFromSubmission(settled)
	require.NoError(t, err)
	assert.Equal(t, archivedResult, frozenDescriptor)

	var idempotent *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		idempotent, inner = ArchiveAndSettleSubmissionTx(tx, confirmed.ID, confirmed.Version, archivedResult, "system-retry")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, settled.Version, idempotent.Version)
	replacement := archivedResult
	replacement.VersionID = "version-2"
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		_, inner := ArchiveAndSettleSubmissionTx(tx, confirmed.ID, confirmed.Version, replacement, "system-retry")
		return inner
	})
	assert.ErrorIs(t, err, ErrTaskSubmissionCASLost)
}

func TestArchivedWxmaasUsageRecordsFrozenProviderCost(t *testing.T) {
	truncate(t)
	const userID, tokenID = 515, 515
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-provider-cost", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "0ae0e5ad-61e8-47b6-aa2f-426965c4880b", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)
	confirmed, err := CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, "provider_task_costed", &model.Task{}, "system")
	require.NoError(t, err)
	require.NotNil(t, confirmed.TaskDBID)
	snapshot := NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "succeeded"
	snapshot.Usage.TotalTokens = 100
	snapshot.Usage.CompletionTokens = 50
	snapshot.ArchiveState = SD2ArchiveStateArchived
	archivedResult := validTestSD2ArchivedResult("sd2-result://private-results/costed-task.mp4", 2048)
	snapshot.ArchivedResult = &archivedResult
	data, err := EncodeSD2TaskResultSnapshot(snapshot)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", *confirmed.TaskDBID).Update("data", data).Error)

	var settled *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		settled, inner = ArchiveAndSettleSubmissionTx(tx, confirmed.ID, confirmed.Version, archivedResult, "system")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionProviderCostRecorded, settled.ProviderCostState)
	require.NotNil(t, settled.ProviderUsageTotalTokens)
	assert.Equal(t, int64(100), *settled.ProviderUsageTotalTokens)
	require.NotNil(t, settled.ProviderCostMicrounits)
	assert.Equal(t, int64(4600), *settled.ProviderCostMicrounits)
	assert.Equal(t, "CNY", settled.ProviderCostCurrency)
}

func TestLookupTaskSubmissionDoesNotRevealAnotherUsersIntent(t *testing.T) {
	truncate(t)
	const userID, tokenID = 510, 510
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-owner", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "46719296-778e-40ea-9fc1-9cc284bdedaf", 1000)
	_, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)

	view, err := GetTaskSubmissionForUser(userID+1, input.ClientRequestID)
	require.NoError(t, err)
	assert.Nil(t, view)
	_, err = LookupTaskSubmission(userID, input.ClientRequestID, strings.Repeat("d", 64))
	assert.True(t, errors.Is(err, ErrTaskSubmissionIdempotencyConflict))
}

func TestProviderCostRejectsNegativeUsageInsteadOfRecordingZero(t *testing.T) {
	truncate(t)
	const userID, tokenID = 511, 511
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-cost", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "fd58bfe4-2e49-4935-a4ee-2d461e0a52ce", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		_, inner := RecordSubmissionProviderCostTx(tx, claimed.ID, claimed.Version, -1, 0, "CNY")
		return inner
	})
	assert.Error(t, err)
	var reloaded model.TaskSubmission
	require.NoError(t, model.DB.First(&reloaded, claimed.ID).Error)
	assert.Equal(t, model.TaskSubmissionProviderCostPending, reloaded.ProviderCostState)
}

func TestProviderCostMustMatchFrozenPriceSnapshot(t *testing.T) {
	truncate(t)
	const userID, tokenID = 516, 516
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-cost-match", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "f5b32ee3-2fb7-4f4f-91cf-f060b12be1bb", 1000)
	submission, _, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		_, inner := RecordSubmissionProviderCostTx(tx, claimed.ID, claimed.Version, 100, 4599, "CNY")
		return inner
	})
	assert.Error(t, err)

	var recorded *model.TaskSubmission
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var inner error
		recorded, inner = RecordSubmissionProviderCostTx(tx, claimed.ID, claimed.Version, 100, 4600, "CNY")
		return inner
	})
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionProviderCostRecorded, recorded.ProviderCostState)
	require.NotNil(t, recorded.ProviderCostMicrounits)
	assert.Equal(t, int64(4600), *recorded.ProviderCostMicrounits)
}

func TestReservationRejectsQuotaBeyondDatabaseBoundary(t *testing.T) {
	input := taskSubmissionTestInput(1, 1, "23566f71-4f30-41d5-b8d4-9fb3e718fd68", common.MaxQuota)
	input.ReservedQuota = common.MaxQuota + 1
	input.PublicPriceSnapshot.ProductQuota = common.MaxQuota + 1
	_, _, err := PrepareTaskSubmission(input)
	assert.Error(t, err)
}

func TestJDProviderCostMayBeFrozenForManualReconciliationButWxmaasMayNot(t *testing.T) {
	input := taskSubmissionTestInput(1, 1, "73d18063-f6c4-47a5-9c66-258d704e3e92", 1000)
	input.Provider = "jd-seedance"
	input.ProviderCostSnapshot.Provider = "jd-seedance"
	input.ProviderCostSnapshot.ProviderPriceKey = "jd/seedance/manual"
	input.ProviderCostSnapshot.UnitPricePerMillionCNY = "0"
	input.ProviderCostSnapshot.RoundingRule = "MANUAL_RECONCILE"
	_, err := normalizePrepareTaskSubmissionInput(input)
	require.NoError(t, err)

	input.Provider = "wxmaas-seedance"
	input.ProviderCostSnapshot.Provider = "wxmaas-seedance"
	_, err = normalizePrepareTaskSubmissionInput(input)
	assert.Error(t, err)
}

func TestPrepareTaskSubmissionFailsClosedWithBatchQuotaButStillReplaysT0(t *testing.T) {
	truncate(t)
	oldBatch := common.BatchUpdateEnabled
	t.Cleanup(func() { common.BatchUpdateEnabled = oldBatch })
	const userID, tokenID = 512, 512
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-submission-batch-guard", 8000)
	input := taskSubmissionTestInput(userID, tokenID, "14ccf266-eedd-4be8-b1fb-e6100e617131", 1000)

	common.BatchUpdateEnabled = true
	_, created, err := PrepareTaskSubmission(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionBatchQuotaUnsafe)
	assert.False(t, created)
	assert.Equal(t, 10000, getUserQuota(t, userID))

	common.BatchUpdateEnabled = false
	original, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	require.True(t, created)

	// Recovery is T0 and must not depend on current channel validity or the
	// batch-update deployment gate.
	common.BatchUpdateEnabled = true
	input.ChannelID = 0
	input.ProviderCostSnapshot = SubmissionProviderCostSnapshot{}
	replayed, created, err := PrepareTaskSubmission(input)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, original.ID, replayed.ID)
}

func TestSubmissionRecoveryNeverResendsAndOnlyRefundsBeforeT2(t *testing.T) {
	t.Run("stale sending becomes unknown and keeps reservation", func(t *testing.T) {
		truncate(t)
		const userID, tokenID = 513, 513
		seedUser(t, userID, 10000)
		seedToken(t, tokenID, userID, "sk-submission-stale-send", 8000)
		input := taskSubmissionTestInput(userID, tokenID, "e8a271db-81ba-4e15-9736-a305059c89c0", 1000)
		submission, _, err := PrepareTaskSubmission(input)
		require.NoError(t, err)
		claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Where("id = ?", claimed.ID).Update("send_deadline_at", time.Now().Add(-time.Second).Unix()).Error)

		updated, err := SweepStaleSendingTaskSubmissions(10)
		require.NoError(t, err)
		assert.Equal(t, 1, updated)
		var reloaded model.TaskSubmission
		require.NoError(t, model.DB.First(&reloaded, claimed.ID).Error)
		assert.Equal(t, model.TaskSubmissionStateUnknown, reloaded.State)
		assert.Equal(t, 1, reloaded.SendAttempts)
		assert.Equal(t, model.TaskSubmissionBillingStateReserved, reloaded.BillingState)
		assert.Equal(t, 9000, getUserQuota(t, userID))
		assert.Equal(t, 7000, getTokenRemainQuota(t, tokenID))
	})

	t.Run("stale prepared is rejected and exactly refunded", func(t *testing.T) {
		truncate(t)
		const userID, tokenID = 514, 514
		seedUser(t, userID, 10000)
		seedToken(t, tokenID, userID, "sk-submission-stale-prepared", 8000)
		input := taskSubmissionTestInput(userID, tokenID, "d9661a4a-1423-4bc9-acf4-1729126fcfb7", 1000)
		submission, _, err := PrepareTaskSubmission(input)
		require.NoError(t, err)
		cutoff := time.Now().Add(-10 * time.Minute).Unix()
		require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Where("id = ?", submission.ID).Update("prepared_at", cutoff-1).Error)

		updated, err := SweepStalePreparedTaskSubmissions(cutoff, 10)
		require.NoError(t, err)
		assert.Equal(t, 1, updated)
		var reloaded model.TaskSubmission
		require.NoError(t, model.DB.First(&reloaded, submission.ID).Error)
		assert.Equal(t, model.TaskSubmissionStateRejected, reloaded.State)
		assert.Zero(t, reloaded.SendAttempts)
		assert.Equal(t, model.TaskSubmissionBillingStateRefunded, reloaded.BillingState)
		assert.Equal(t, 10000, getUserQuota(t, userID))
		assert.Equal(t, 8000, getTokenRemainQuota(t, tokenID))
	})
}

func TestAdminReconciliationResolvesUnknownWithoutAnotherProviderSend(t *testing.T) {
	t.Run("confirmed evidence binds the original task", func(t *testing.T) {
		truncate(t)
		const userID, tokenID = 516, 516
		seedUser(t, userID, 10000)
		seedToken(t, tokenID, userID, "sk-submission-reconcile-confirm", 8000)
		input := taskSubmissionTestInput(userID, tokenID, "ebd7bcbf-686a-45d3-9b3d-a053b6792178", 1000)
		submission, _, err := PrepareTaskSubmission(input)
		require.NoError(t, err)
		claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
		require.NoError(t, err)
		require.True(t, won)
		unknown, err := MarkSubmissionUnknown(claimed.ID, claimed.Version, "response_lost", "provider outcome requires reconciliation", nil, "")
		require.NoError(t, err)

		confirmed, err := ReconcileUnknownSubmissionConfirmed(unknown.ID, unknown.Version, "provider_task_recovered", &model.Task{}, 42)
		require.NoError(t, err)
		assert.Equal(t, model.TaskSubmissionStateConfirmed, confirmed.State)
		assert.Equal(t, 1, confirmed.SendAttempts)
		assert.Equal(t, "admin:42", confirmed.ReconciledBy)
		require.NotNil(t, confirmed.ReconciledAt)
		require.NotNil(t, confirmed.TaskDBID)
	})

	t.Run("not-created evidence refunds frozen balances", func(t *testing.T) {
		truncate(t)
		const userID, tokenID = 517, 517
		seedUser(t, userID, 10000)
		seedToken(t, tokenID, userID, "sk-submission-reconcile-reject", 8000)
		input := taskSubmissionTestInput(userID, tokenID, "3d4f8138-b0d4-4b3a-b265-03d4017747de", 1000)
		submission, _, err := PrepareTaskSubmission(input)
		require.NoError(t, err)
		claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
		require.NoError(t, err)
		require.True(t, won)
		unknown, err := MarkSubmissionUnknown(claimed.ID, claimed.Version, "response_lost", "provider outcome requires reconciliation", nil, "")
		require.NoError(t, err)

		rejected, err := ReconcileUnknownSubmissionRejected(unknown.ID, unknown.Version, "documented_not_created", 42)
		require.NoError(t, err)
		assert.Equal(t, model.TaskSubmissionStateRejected, rejected.State)
		assert.Equal(t, model.TaskSubmissionBillingStateRefunded, rejected.BillingState)
		assert.Equal(t, "admin:42", rejected.ReconciledBy)
		assert.Equal(t, 10000, getUserQuota(t, userID))
		assert.Equal(t, 8000, getTokenRemainQuota(t, tokenID))
	})
}
