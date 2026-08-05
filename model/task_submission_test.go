package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func minimalTaskSubmission(userID int, clientRequestID string, publicTaskID string, billingRequestID string) *TaskSubmission {
	return &TaskSubmission{
		UserID:                userID,
		ClientRequestID:       clientRequestID,
		RequestDigest:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DigestVersion:         "v1",
		PublicTaskID:          publicTaskID,
		State:                 TaskSubmissionStatePrepared,
		PollState:             TaskSubmissionPollStateNone,
		ChannelID:             1,
		Provider:              "test-provider",
		OriginModelName:       "verdantflare-sd2",
		UpstreamModelName:     "upstream-model",
		ReservedQuota:         1,
		BillingState:          TaskSubmissionBillingStateReserved,
		BillingRequestID:      billingRequestID,
		ProviderCostState:     TaskSubmissionProviderCostNotApplicable,
		ArchiveState:          TaskSubmissionArchiveStateNone,
		ResultSnapshotVersion: 1,
	}
}

func TestTaskSubmissionSchemaEnforcesIntentIdentityAndAllowsNullProviderIDs(t *testing.T) {
	truncateTables(t)
	first := minimalTaskSubmission(701, "request-a", "task_schema_a", "billing-schema-a")
	require.NoError(t, DB.Create(first).Error)

	duplicateIntent := minimalTaskSubmission(701, "request-a", "task_schema_b", "billing-schema-b")
	assert.Error(t, DB.Create(duplicateIntent).Error)

	// All three supported databases allow multiple NULLs in this composite
	// unique key; only confirmed non-NULL upstream IDs must be unique per channel.
	secondNullProviderID := minimalTaskSubmission(702, "request-b", "task_schema_c", "billing-schema-c")
	require.NoError(t, DB.Create(secondNullProviderID).Error)
}

func TestTaskSubmissionSchemaContainsFrozenArchiveDescriptor(t *testing.T) {
	for _, column := range []string{
		"archive_schema_version",
		"archived_result_ref",
		"archived_result_version_id",
		"archived_result_sha256",
		"archived_result_size",
		"archived_result_content_type",
		"archived_result_etag",
	} {
		assert.Truef(t, DB.Migrator().HasColumn(&TaskSubmission{}, column), "missing archive descriptor column %s", column)
	}
}

func TestTaskSubmissionBillingEntriesAreAppendOnly(t *testing.T) {
	truncateTables(t)
	entry := &TaskSubmissionBillingEntry{
		SubmissionID:       1,
		BillingRequestID:   "billing-immutable",
		Operation:          TaskSubmissionBillingOperationReserve,
		Sequence:           1,
		UserID:             1,
		BillingStateBefore: TaskSubmissionBillingStateNone,
		BillingStateAfter:  TaskSubmissionBillingStateReserved,
	}
	require.NoError(t, DB.Create(entry).Error)
	err := DB.Model(entry).Update("reason_code", "rewritten").Error
	assert.ErrorIs(t, err, ErrTaskSubmissionBillingEntryImmutable)

	var reloaded TaskSubmissionBillingEntry
	require.NoError(t, DB.First(&reloaded, entry.ID).Error)
	assert.Empty(t, reloaded.ReasonCode)
}

func TestCASTaskSubmissionPollStateOnlyMutatesConfirmedSubmission(t *testing.T) {
	truncateTables(t)
	submission := minimalTaskSubmission(703, "request-c", "task_schema_d", "billing-schema-d")
	require.NoError(t, DB.Create(submission).Error)

	won, err := CASTaskSubmissionPollState(submission.ID, submission.Version, TaskSubmissionPollStateNone, TaskSubmissionPollStateBackoff, nil, "temporary_query_failure")
	require.NoError(t, err)
	assert.False(t, won)

	require.NoError(t, DB.Model(&TaskSubmission{}).Where("id = ?", submission.ID).Updates(map[string]any{
		"state":      TaskSubmissionStateConfirmed,
		"poll_state": TaskSubmissionPollStateActive,
	}).Error)
	won, err = CASTaskSubmissionPollState(submission.ID, submission.Version, TaskSubmissionPollStateActive, TaskSubmissionPollStateBackoff, nil, "temporary_query_failure")
	require.NoError(t, err)
	assert.True(t, won)

	var reloaded TaskSubmission
	require.NoError(t, DB.First(&reloaded, submission.ID).Error)
	assert.Equal(t, TaskSubmissionPollStateBackoff, reloaded.PollState)
	assert.Equal(t, submission.Version+1, reloaded.Version)
}
