package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	TaskSubmissionReconcileDecisionNotCreated = "not_created"
	TaskSubmissionReconcileDecisionBind       = "bind"

	TaskSubmissionReconcileReviewPending          = "PENDING"
	TaskSubmissionReconcileReviewRequiresEvidence = "REQUIRES_EVIDENCE"
	TaskSubmissionReconcileReviewApplied          = "APPLIED"
	TaskSubmissionReconcileReviewFailed           = "FAILED"
)

// TaskSubmissionReconciliationReview is the durable, admin-only audit trail
// for UNKNOWN task decisions. Admin identity always comes from the authenticated
// request context; it is never accepted from the request body.
type TaskSubmissionReconciliationReview struct {
	ID                     int64  `json:"id" gorm:"primaryKey"`
	SubmissionID           int64  `json:"submission_id" gorm:"index;uniqueIndex:idx_submission_reconcile_reviewer,priority:1"`
	ExpectedVersion        int64  `json:"expected_version" gorm:"bigint;uniqueIndex:idx_submission_reconcile_reviewer,priority:2"`
	Decision               string `json:"decision" gorm:"type:varchar(32);uniqueIndex:idx_submission_reconcile_reviewer,priority:3"`
	UpstreamTaskID         string `json:"upstream_task_id,omitempty" gorm:"type:varchar(191);uniqueIndex:idx_submission_reconcile_reviewer,priority:4"`
	AdminID                int    `json:"admin_id" gorm:"index;uniqueIndex:idx_submission_reconcile_reviewer,priority:5"`
	Evidence               string `json:"evidence" gorm:"type:text"`
	Reason                 string `json:"reason" gorm:"type:varchar(512)"`
	ProviderRequestID      string `json:"provider_request_id,omitempty" gorm:"type:varchar(191)"`
	StrongCorrelation      bool   `json:"strong_correlation"`
	Status                 string `json:"status" gorm:"type:varchar(32);index"`
	SafeErrorCode          string `json:"error_code,omitempty" gorm:"type:varchar(128)"`
	SubmissionVersionAfter *int64 `json:"submission_version_after,omitempty" gorm:"bigint"`
	CreatedAt              int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt              int64  `json:"updated_at" gorm:"bigint"`
}

func (r *TaskSubmissionReconciliationReview) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = now
	}
	if r.Status == "" {
		r.Status = TaskSubmissionReconcileReviewPending
	}
	return nil
}

func ListTaskSubmissionBillingEntries(submissionID int64) ([]*TaskSubmissionBillingEntry, error) {
	var entries []*TaskSubmissionBillingEntry
	err := DB.Where("submission_id = ?", submissionID).Order("id asc").Find(&entries).Error
	return entries, err
}

func ListTaskSubmissionReconciliationReviews(submissionID int64) ([]*TaskSubmissionReconciliationReview, error) {
	var reviews []*TaskSubmissionReconciliationReview
	err := DB.Where("submission_id = ?", submissionID).Order("id asc").Find(&reviews).Error
	return reviews, err
}

func CreateTaskSubmissionReconciliationReview(review *TaskSubmissionReconciliationReview) error {
	if review == nil {
		return errors.New("nil task submission reconciliation review")
	}
	return DB.Create(review).Error
}

func GetTaskSubmissionReconciliationReview(submissionID int64, expectedVersion int64, decision string, upstreamTaskID string, adminID int) (*TaskSubmissionReconciliationReview, error) {
	var review TaskSubmissionReconciliationReview
	result := DB.Where("submission_id = ? AND expected_version = ? AND decision = ? AND upstream_task_id = ? AND admin_id = ?", submissionID, expectedVersion, decision, upstreamTaskID, adminID).
		Limit(1).Find(&review)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &review, nil
}

func CountDistinctTaskSubmissionReviewers(submissionID int64, expectedVersion int64, decision string, upstreamTaskID string) (int64, error) {
	var count int64
	err := DB.Model(&TaskSubmissionReconciliationReview{}).
		Where("submission_id = ? AND expected_version = ? AND decision = ? AND upstream_task_id = ? AND status IN ?", submissionID, expectedVersion, decision, upstreamTaskID, []string{TaskSubmissionReconcileReviewPending, TaskSubmissionReconcileReviewApplied}).
		Distinct("admin_id").Count(&count).Error
	return count, err
}

func UpdateTaskSubmissionReconciliationReview(id int64, status string, errorCode string, versionAfter *int64) error {
	return DB.Model(&TaskSubmissionReconciliationReview{}).Where("id = ?", id).Updates(map[string]any{
		"status":                   status,
		"safe_error_code":          errorCode,
		"submission_version_after": versionAfter,
		"updated_at":               common.GetTimestamp(),
	}).Error
}

func MarkTaskSubmissionReconciliationReviewVerified(id int64, strongCorrelation bool) error {
	return DB.Model(&TaskSubmissionReconciliationReview{}).Where("id = ?", id).Updates(map[string]any{
		"strong_correlation": strongCorrelation,
		"status":             TaskSubmissionReconcileReviewPending,
		"safe_error_code":    "",
		"updated_at":         common.GetTimestamp(),
	}).Error
}

func UpdateMatchingTaskSubmissionReconciliationReviews(submissionID int64, expectedVersion int64, decision string, upstreamTaskID string, status string, versionAfter *int64) error {
	return DB.Model(&TaskSubmissionReconciliationReview{}).
		Where("submission_id = ? AND expected_version = ? AND decision = ? AND upstream_task_id = ?", submissionID, expectedVersion, decision, upstreamTaskID).
		Updates(map[string]any{
			"status":                   status,
			"safe_error_code":          "",
			"submission_version_after": versionAfter,
			"updated_at":               common.GetTimestamp(),
		}).Error
}
