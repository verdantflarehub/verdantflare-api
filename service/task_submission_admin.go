package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

var (
	ErrTaskSubmissionReconcileInput   = errors.New("invalid task submission reconciliation input")
	ErrTaskSubmissionReviewPending    = errors.New("task submission reconciliation requires another admin review")
	ErrTaskSubmissionEvidenceRequired = errors.New("task submission reconciliation requires verified structured evidence")
)

// TaskSubmissionEvidenceVerifier independently verifies provider facts. Human
// review text is audit context only and must never be treated as proof.
type TaskSubmissionEvidenceVerifier interface {
	VerifyTaskSubmissionReconciliation(context.Context, *model.TaskSubmission, AdminTaskSubmissionReconcileInput) (*TaskSubmissionEvidenceVerification, error)
}

type TaskSubmissionEvidenceVerification struct {
	Decision               string
	UpstreamTaskID         string
	ProviderRequestID      string
	OriginalChannelAccount bool
	UniqueMatch            bool
	FrozenFingerprintMatch bool
	ProviderRequestIDMatch bool
	NotCreatedVerified     bool
}

var taskSubmissionEvidenceVerifier struct {
	sync.RWMutex
	verifier TaskSubmissionEvidenceVerifier
}

// SetTaskSubmissionEvidenceVerifier installs the provider-specific verifier.
// Passing nil restores the fail-closed default.
func SetTaskSubmissionEvidenceVerifier(verifier TaskSubmissionEvidenceVerifier) {
	taskSubmissionEvidenceVerifier.Lock()
	taskSubmissionEvidenceVerifier.verifier = verifier
	taskSubmissionEvidenceVerifier.Unlock()
}

func getTaskSubmissionEvidenceVerifier() TaskSubmissionEvidenceVerifier {
	taskSubmissionEvidenceVerifier.RLock()
	defer taskSubmissionEvidenceVerifier.RUnlock()
	return taskSubmissionEvidenceVerifier.verifier
}

type AdminTaskSubmissionReconcileInput struct {
	Context           context.Context
	SubmissionID      int64
	ExpectedVersion   int64
	Decision          string
	Evidence          string
	Reason            string
	UpstreamTaskID    string
	ProviderRequestID string
	AdminID           int
}

type AdminTaskSubmissionReconcileResult struct {
	Submission    *model.TaskSubmission
	Review        *model.TaskSubmissionReconciliationReview
	PendingReview bool
	ReviewerCount int64
}

// ReconcileTaskSubmissionAdmin records the authenticated administrator's own
// review before applying a decision. A bind requires either an exact match to
// the provider request ID already persisted by the poller, or reviews from two
// distinct authenticated administrators for the same version and upstream ID.
func ReconcileTaskSubmissionAdmin(input AdminTaskSubmissionReconcileInput) (*AdminTaskSubmissionReconcileResult, error) {
	input.Decision = strings.TrimSpace(input.Decision)
	input.Evidence = strings.TrimSpace(input.Evidence)
	input.Reason = strings.TrimSpace(input.Reason)
	input.UpstreamTaskID = strings.TrimSpace(input.UpstreamTaskID)
	input.ProviderRequestID = strings.TrimSpace(input.ProviderRequestID)
	if input.SubmissionID <= 0 || input.ExpectedVersion <= 0 || input.AdminID <= 0 || input.Evidence == "" || input.Reason == "" || len(input.Evidence) > 4000 || len(input.Reason) > 512 {
		return nil, ErrTaskSubmissionReconcileInput
	}
	if input.Decision != model.TaskSubmissionReconcileDecisionNotCreated && input.Decision != model.TaskSubmissionReconcileDecisionBind {
		return nil, ErrTaskSubmissionReconcileInput
	}
	if input.Decision == model.TaskSubmissionReconcileDecisionBind && input.UpstreamTaskID == "" {
		return nil, ErrTaskSubmissionReconcileInput
	}
	if input.Decision == model.TaskSubmissionReconcileDecisionNotCreated && input.UpstreamTaskID != "" {
		return nil, ErrTaskSubmissionReconcileInput
	}

	submission, err := model.GetTaskSubmissionByID(input.SubmissionID)
	if err != nil {
		return nil, err
	}
	if submission == nil {
		return nil, ErrTaskSubmissionNotFound
	}
	if input.Decision == model.TaskSubmissionReconcileDecisionNotCreated &&
		submission.State == model.TaskSubmissionStateRejected &&
		submission.BillingState == model.TaskSubmissionBillingStateRefunded {
		return &AdminTaskSubmissionReconcileResult{Submission: submission, ReviewerCount: 1}, nil
	}
	if submission.State != model.TaskSubmissionStateUnknown || submission.Version != input.ExpectedVersion {
		return nil, ErrTaskSubmissionCASLost
	}
	review := &model.TaskSubmissionReconciliationReview{
		SubmissionID:      submission.ID,
		ExpectedVersion:   input.ExpectedVersion,
		Decision:          input.Decision,
		UpstreamTaskID:    input.UpstreamTaskID,
		AdminID:           input.AdminID,
		Evidence:          input.Evidence,
		Reason:            input.Reason,
		ProviderRequestID: input.ProviderRequestID,
		StrongCorrelation: false,
		Status:            model.TaskSubmissionReconcileReviewPending,
	}
	if err := model.CreateTaskSubmissionReconciliationReview(review); err != nil {
		existing, lookupErr := model.GetTaskSubmissionReconciliationReview(submission.ID, input.ExpectedVersion, input.Decision, input.UpstreamTaskID, input.AdminID)
		if lookupErr != nil || existing == nil {
			return nil, err
		}
		review = existing
	}

	verifier := getTaskSubmissionEvidenceVerifier()
	if verifier == nil {
		_ = model.UpdateTaskSubmissionReconciliationReview(review.ID, model.TaskSubmissionReconcileReviewRequiresEvidence, "evidence_verifier_unavailable", nil)
		return &AdminTaskSubmissionReconcileResult{Submission: submission, Review: review, PendingReview: true}, ErrTaskSubmissionEvidenceRequired
	}
	verificationContext := input.Context
	if verificationContext == nil {
		verificationContext = context.Background()
	}
	verification, verifyErr := verifier.VerifyTaskSubmissionReconciliation(verificationContext, submission, input)
	if verifyErr != nil || !taskSubmissionEvidenceIsSufficient(submission, input, verification) {
		errorCode := "evidence_not_verified"
		if verifyErr != nil {
			errorCode = "evidence_verification_failed"
		}
		_ = model.UpdateTaskSubmissionReconciliationReview(review.ID, model.TaskSubmissionReconcileReviewRequiresEvidence, errorCode, nil)
		return &AdminTaskSubmissionReconcileResult{Submission: submission, Review: review, PendingReview: true}, ErrTaskSubmissionEvidenceRequired
	}
	strongCorrelation := input.Decision == model.TaskSubmissionReconcileDecisionBind && verification.ProviderRequestIDMatch
	review.StrongCorrelation = strongCorrelation
	if err := model.MarkTaskSubmissionReconciliationReviewVerified(review.ID, strongCorrelation); err != nil {
		return nil, err
	}

	if input.Decision == model.TaskSubmissionReconcileDecisionNotCreated {
		resolved, reconcileErr := ReconcileUnknownSubmissionRejected(submission.ID, input.ExpectedVersion, "admin_confirmed_not_created", input.AdminID)
		if reconcileErr != nil {
			_ = model.UpdateTaskSubmissionReconciliationReview(review.ID, model.TaskSubmissionReconcileReviewFailed, safeReconcileErrorCode(reconcileErr), nil)
			return nil, reconcileErr
		}
		versionAfter := resolved.Version
		_ = model.UpdateMatchingTaskSubmissionReconciliationReviews(submission.ID, input.ExpectedVersion, input.Decision, "", model.TaskSubmissionReconcileReviewApplied, &versionAfter)
		return &AdminTaskSubmissionReconcileResult{Submission: resolved, Review: review, ReviewerCount: 1}, nil
	}

	reviewerCount, err := model.CountDistinctTaskSubmissionReviewers(submission.ID, input.ExpectedVersion, input.Decision, input.UpstreamTaskID)
	if err != nil {
		return nil, err
	}
	if !strongCorrelation && reviewerCount < 2 {
		return &AdminTaskSubmissionReconcileResult{
			Submission: submission, Review: review, PendingReview: true, ReviewerCount: reviewerCount,
		}, ErrTaskSubmissionReviewPending
	}

	task, err := newReconciledSD2Task(submission)
	if err != nil {
		_ = model.UpdateTaskSubmissionReconciliationReview(review.ID, model.TaskSubmissionReconcileReviewFailed, "task_snapshot_failed", nil)
		return nil, err
	}
	resolved, reconcileErr := ReconcileUnknownSubmissionConfirmed(submission.ID, input.ExpectedVersion, input.UpstreamTaskID, task, input.AdminID)
	if reconcileErr != nil {
		_ = model.UpdateTaskSubmissionReconciliationReview(review.ID, model.TaskSubmissionReconcileReviewFailed, safeReconcileErrorCode(reconcileErr), nil)
		return nil, reconcileErr
	}
	versionAfter := resolved.Version
	_ = model.UpdateMatchingTaskSubmissionReconciliationReviews(submission.ID, input.ExpectedVersion, input.Decision, input.UpstreamTaskID, model.TaskSubmissionReconcileReviewApplied, &versionAfter)
	return &AdminTaskSubmissionReconcileResult{Submission: resolved, Review: review, ReviewerCount: reviewerCount}, nil
}

func taskSubmissionEvidenceIsSufficient(submission *model.TaskSubmission, input AdminTaskSubmissionReconcileInput, verification *TaskSubmissionEvidenceVerification) bool {
	if submission == nil || verification == nil || verification.Decision != input.Decision || !verification.OriginalChannelAccount {
		return false
	}
	if input.Decision == model.TaskSubmissionReconcileDecisionNotCreated {
		return verification.NotCreatedVerified
	}
	if !verification.UniqueMatch || verification.UpstreamTaskID == "" || verification.UpstreamTaskID != input.UpstreamTaskID {
		return false
	}
	providerRequestMatch := verification.ProviderRequestIDMatch &&
		submission.ProviderRequestID != "" && verification.ProviderRequestID == submission.ProviderRequestID &&
		input.ProviderRequestID == submission.ProviderRequestID
	return providerRequestMatch || verification.FrozenFingerprintMatch
}

func newReconciledSD2Task(submission *model.TaskSubmission) (*model.Task, error) {
	snapshot := NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "queued"
	data, err := EncodeSD2TaskResultSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	group := ""
	if user, lookupErr := model.GetUserById(submission.UserID, false); lookupErr == nil && user != nil {
		group = user.Group
	}
	task := &model.Task{
		TaskID:     submission.PublicTaskID,
		Platform:   constant.TaskPlatform(fmt.Sprintf("%d", submission.ChannelType)),
		UserId:     submission.UserID,
		Group:      group,
		ChannelId:  submission.ChannelID,
		Quota:      submission.ReservedQuota,
		Action:     constant.TaskActionGenerate,
		Status:     model.TaskStatusSubmitted,
		SubmitTime: common.GetTimestamp(),
		Progress:   "0%",
		Properties: model.Properties{
			OriginModelName:   submission.OriginModelName,
			UpstreamModelName: submission.UpstreamModelName,
		},
		PrivateData: model.TaskPrivateData{
			NodeName:      common.NodeName,
			BillingSource: submission.BillingSource,
			BillingContext: &model.TaskBillingContext{
				OriginModelName: submission.OriginModelName,
				PerCallBilling:  true,
			},
		},
		Data: data,
	}
	return task, nil
}

func safeReconcileErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrTaskSubmissionCASLost):
		return "version_conflict"
	case errors.Is(err, ErrTaskSubmissionNotFound):
		return "not_found"
	case errors.Is(err, ErrTaskSubmissionBalanceReconcile):
		return "billing_reconcile_required"
	default:
		return "reconcile_failed"
	}
}
