package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskSubmissionEvidenceVerifierFunc func(context.Context, *model.TaskSubmission, AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error)

func (fn taskSubmissionEvidenceVerifierFunc) VerifyTaskSubmissionReconciliation(ctx context.Context, submission *model.TaskSubmission, input AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error) {
	return fn(ctx, submission, input)
}

func installTaskSubmissionEvidenceVerifier(t *testing.T, verifier TaskSubmissionEvidenceVerifier) {
	t.Helper()
	SetTaskSubmissionEvidenceVerifier(verifier)
	t.Cleanup(func() { SetTaskSubmissionEvidenceVerifier(nil) })
}

func prepareUnknownSubmission(t *testing.T, userID int, tokenID int, requestID string) *model.TaskSubmission {
	t.Helper()
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-admin-reconcile", 10000)
	submission, created, err := PrepareTaskSubmission(taskSubmissionTestInput(userID, tokenID, requestID, 1000))
	require.NoError(t, err)
	require.True(t, created)
	claimed, won, err := AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, won)
	unknown, err := MarkSubmissionUnknown(claimed.ID, claimed.Version, "provider_response_unknown", "provider outcome unknown", nil, "")
	require.NoError(t, err)
	return unknown
}

func TestAdminBindRequiresTwoDistinctAuthenticatedReviewers(t *testing.T) {
	truncate(t)
	installTaskSubmissionEvidenceVerifier(t, taskSubmissionEvidenceVerifierFunc(func(_ context.Context, _ *model.TaskSubmission, input AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error) {
		return &TaskSubmissionEvidenceVerification{
			Decision: input.Decision, UpstreamTaskID: input.UpstreamTaskID,
			OriginalChannelAccount: true, UniqueMatch: true, FrozenFingerprintMatch: true,
		}, nil
	}))
	submission := prepareUnknownSubmission(t, 801, 801, "c2be8130-8e8f-4e55-80da-68ee15d53e55")
	input := AdminTaskSubmissionReconcileInput{
		SubmissionID: submission.ID, ExpectedVersion: submission.Version,
		Decision: model.TaskSubmissionReconcileDecisionBind,
		Evidence: "provider console inspection", Reason: "task exists",
		UpstreamTaskID: "provider_task_two_reviewers", AdminID: 11,
	}

	result, err := ReconcileTaskSubmissionAdmin(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionReviewPending)
	require.NotNil(t, result)
	assert.True(t, result.PendingReview)
	assert.Equal(t, int64(1), result.ReviewerCount)

	result, err = ReconcileTaskSubmissionAdmin(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionReviewPending)
	require.NotNil(t, result)
	assert.Equal(t, int64(1), result.ReviewerCount)

	input.AdminID = 12
	result, err = ReconcileTaskSubmissionAdmin(input)
	require.NoError(t, err)
	require.NotNil(t, result.Submission)
	assert.Equal(t, model.TaskSubmissionStateConfirmed, result.Submission.State)
	assert.Equal(t, int64(2), result.ReviewerCount)

	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Where("task_id = ?", submission.PublicTaskID).Count(&taskCount).Error)
	assert.Equal(t, int64(1), taskCount)
}

func TestAdminBindAllowsPersistedProviderRequestCorrelation(t *testing.T) {
	truncate(t)
	installTaskSubmissionEvidenceVerifier(t, taskSubmissionEvidenceVerifierFunc(func(_ context.Context, submission *model.TaskSubmission, input AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error) {
		return &TaskSubmissionEvidenceVerification{
			Decision: input.Decision, UpstreamTaskID: input.UpstreamTaskID,
			ProviderRequestID: submission.ProviderRequestID, OriginalChannelAccount: true,
			UniqueMatch: true, ProviderRequestIDMatch: true,
		}, nil
	}))
	submission := prepareUnknownSubmission(t, 802, 802, "843edb4a-ff2f-4f8d-84ab-b6df01d3478d")
	require.NoError(t, model.DB.Model(&model.TaskSubmission{}).Where("id = ?", submission.ID).Update("provider_request_id", "request-from-provider").Error)

	result, err := ReconcileTaskSubmissionAdmin(AdminTaskSubmissionReconcileInput{
		SubmissionID: submission.ID, ExpectedVersion: submission.Version,
		Decision: model.TaskSubmissionReconcileDecisionBind,
		Evidence: "matched provider request log", Reason: "exact request correlation",
		UpstreamTaskID: "provider_task_correlated", ProviderRequestID: "request-from-provider", AdminID: 21,
	})
	require.NoError(t, err)
	assert.False(t, result.PendingReview)
	assert.Equal(t, model.TaskSubmissionStateConfirmed, result.Submission.State)
}

func TestAdminNotCreatedUsesIdempotentLedgerRefund(t *testing.T) {
	truncate(t)
	installTaskSubmissionEvidenceVerifier(t, taskSubmissionEvidenceVerifierFunc(func(_ context.Context, _ *model.TaskSubmission, input AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error) {
		return &TaskSubmissionEvidenceVerification{
			Decision: input.Decision, OriginalChannelAccount: true, NotCreatedVerified: true,
		}, nil
	}))
	submission := prepareUnknownSubmission(t, 803, 803, "d76f1d89-fabc-43a0-8bff-c09ef9ba2d55")
	input := AdminTaskSubmissionReconcileInput{
		SubmissionID: submission.ID, ExpectedVersion: submission.Version,
		Decision: model.TaskSubmissionReconcileDecisionNotCreated,
		Evidence: "provider search returned no task", Reason: "confirmed absent", AdminID: 31,
	}
	result, err := ReconcileTaskSubmissionAdmin(input)
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionBillingStateRefunded, result.Submission.BillingState)
	assert.Equal(t, 10000, getUserQuota(t, 803))
	assert.Equal(t, 10000, getTokenRemainQuota(t, 803))

	replayed, err := ReconcileTaskSubmissionAdmin(input)
	require.NoError(t, err)
	assert.Equal(t, model.TaskSubmissionBillingStateRefunded, replayed.Submission.BillingState)
	assert.Equal(t, 10000, getUserQuota(t, 803))
}

func TestAdminReconcileFailsClosedWithoutStructuredEvidenceVerifier(t *testing.T) {
	truncate(t)
	SetTaskSubmissionEvidenceVerifier(nil)
	submission := prepareUnknownSubmission(t, 804, 804, "51f76e2f-dbf9-44cc-b410-d919f813c062")

	result, err := ReconcileTaskSubmissionAdmin(AdminTaskSubmissionReconcileInput{
		SubmissionID: submission.ID, ExpectedVersion: submission.Version,
		Decision: model.TaskSubmissionReconcileDecisionNotCreated,
		Evidence: "free-form claim", Reason: "unverified", AdminID: 41,
	})
	assert.ErrorIs(t, err, ErrTaskSubmissionEvidenceRequired)
	require.NotNil(t, result)
	assert.Equal(t, model.TaskSubmissionStateUnknown, result.Submission.State)
	assert.Equal(t, 9000, getUserQuota(t, 804))

	reviews, listErr := model.ListTaskSubmissionReconciliationReviews(submission.ID)
	require.NoError(t, listErr)
	require.Len(t, reviews, 1)
	assert.Equal(t, model.TaskSubmissionReconcileReviewRequiresEvidence, reviews[0].Status)
}

func TestAdminBindRejectsVerifierWithoutUniqueFingerprintOrProviderMatch(t *testing.T) {
	truncate(t)
	installTaskSubmissionEvidenceVerifier(t, taskSubmissionEvidenceVerifierFunc(func(_ context.Context, _ *model.TaskSubmission, input AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error) {
		return &TaskSubmissionEvidenceVerification{
			Decision: input.Decision, UpstreamTaskID: input.UpstreamTaskID,
			OriginalChannelAccount: true, UniqueMatch: true,
		}, nil
	}))
	submission := prepareUnknownSubmission(t, 805, 805, "aa364ffa-3a78-4598-bddc-1735b6218aa2")
	input := AdminTaskSubmissionReconcileInput{
		SubmissionID: submission.ID, ExpectedVersion: submission.Version,
		Decision: model.TaskSubmissionReconcileDecisionBind,
		Evidence: "two admins agree", Reason: "but no fact proof",
		UpstreamTaskID: "arbitrary-task", AdminID: 51,
	}
	_, err := ReconcileTaskSubmissionAdmin(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionEvidenceRequired)
	input.AdminID = 52
	_, err = ReconcileTaskSubmissionAdmin(input)
	assert.ErrorIs(t, err, ErrTaskSubmissionEvidenceRequired)

	current, getErr := model.GetTaskSubmissionByID(submission.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskSubmissionStateUnknown, current.State)
	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Where("task_id = ?", submission.PublicTaskID).Count(&taskCount).Error)
	assert.Zero(t, taskCount)
}
