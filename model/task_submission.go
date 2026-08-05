package model

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

type TaskSubmissionState string

const (
	TaskSubmissionStatePrepared  TaskSubmissionState = "PREPARED"
	TaskSubmissionStateSending   TaskSubmissionState = "SENDING"
	TaskSubmissionStateConfirmed TaskSubmissionState = "CONFIRMED"
	TaskSubmissionStateRejected  TaskSubmissionState = "REJECTED"
	TaskSubmissionStateUnknown   TaskSubmissionState = "UNKNOWN"
)

type TaskSubmissionPollState string

const (
	TaskSubmissionPollStateNone             TaskSubmissionPollState = "NONE"
	TaskSubmissionPollStateActive           TaskSubmissionPollState = "ACTIVE"
	TaskSubmissionPollStateBackoff          TaskSubmissionPollState = "BACKOFF"
	TaskSubmissionPollStatePausedCredential TaskSubmissionPollState = "PAUSED_CREDENTIAL"
	TaskSubmissionPollStateStale            TaskSubmissionPollState = "STALE"
	TaskSubmissionPollStateTerminal         TaskSubmissionPollState = "TERMINAL"
)

type TaskSubmissionBillingState string

const (
	TaskSubmissionBillingStateNone              TaskSubmissionBillingState = "NONE"
	TaskSubmissionBillingStateReserved          TaskSubmissionBillingState = "RESERVED"
	TaskSubmissionBillingStateCommitted         TaskSubmissionBillingState = "COMMITTED"
	TaskSubmissionBillingStateSettled           TaskSubmissionBillingState = "SETTLED"
	TaskSubmissionBillingStateRefunded          TaskSubmissionBillingState = "REFUNDED"
	TaskSubmissionBillingStateReconcileRequired TaskSubmissionBillingState = "RECONCILE_REQUIRED"
)

type TaskSubmissionProviderCostState string

const (
	TaskSubmissionProviderCostNotApplicable     TaskSubmissionProviderCostState = "NOT_APPLICABLE"
	TaskSubmissionProviderCostPending           TaskSubmissionProviderCostState = "PENDING"
	TaskSubmissionProviderCostRecorded          TaskSubmissionProviderCostState = "RECORDED"
	TaskSubmissionProviderCostReconcileRequired TaskSubmissionProviderCostState = "RECONCILE_REQUIRED"
)

type TaskSubmissionArchiveState string

const (
	TaskSubmissionArchiveStateNone     TaskSubmissionArchiveState = "NONE"
	TaskSubmissionArchiveStatePending  TaskSubmissionArchiveState = "PENDING"
	TaskSubmissionArchiveStateArchived TaskSubmissionArchiveState = "ARCHIVED"
	TaskSubmissionArchiveStateFailed   TaskSubmissionArchiveState = "FAILED"
)

type TaskSubmissionBillingOperation string

const (
	TaskSubmissionBillingOperationReserve TaskSubmissionBillingOperation = "RESERVE"
	TaskSubmissionBillingOperationCommit  TaskSubmissionBillingOperation = "COMMIT"
	TaskSubmissionBillingOperationSettle  TaskSubmissionBillingOperation = "SETTLE"
	TaskSubmissionBillingOperationRefund  TaskSubmissionBillingOperation = "REFUND"
	TaskSubmissionBillingOperationAdjust  TaskSubmissionBillingOperation = "ADJUST"
)

const (
	QuotaCacheAggregateUserBilling = "user_billing"
)

var ErrTaskSubmissionBillingEntryImmutable = errors.New("task submission billing entries are immutable")

// TaskSubmission is the durable side-effect fence for one asynchronous create
// intent. It deliberately exists before a provider task ID does, so a lost
// provider response never makes a second create look safe.
//
// Sensitive request material, provider keys, prompts and signed media URLs must
// not be stored in this row. The JSON-shaped fields below contain only
// versioned, sanitized snapshots and are TEXT for SQLite/MySQL/PostgreSQL
// compatibility.
type TaskSubmission struct {
	ID        int64 `json:"id" gorm:"primaryKey"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;index"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint;index;index:idx_task_submission_state_updated,priority:2;index:idx_task_submission_poll_updated,priority:2;index:idx_task_submission_billing_updated,priority:2;index:idx_task_submission_cost_updated,priority:2"`
	Version   int64 `json:"version" gorm:"bigint;not null"`

	UserID          int    `json:"user_id" gorm:"index;uniqueIndex:idx_task_submission_user_request,priority:1"`
	ClientRequestID string `json:"client_request_id" gorm:"type:varchar(64);uniqueIndex:idx_task_submission_user_request,priority:2"`
	RequestDigest   string `json:"-" gorm:"type:char(64);not null"`
	DigestVersion   string `json:"-" gorm:"type:varchar(32);not null"`
	RequestSummary  string `json:"request_summary,omitempty" gorm:"type:text"`

	ProviderRequestDigest string                  `json:"-" gorm:"type:char(64)"`
	PublicTaskID          string                  `json:"public_task_id" gorm:"type:varchar(191);uniqueIndex"`
	State                 TaskSubmissionState     `json:"state" gorm:"type:varchar(32);index;index:idx_task_submission_state_updated,priority:1"`
	PollState             TaskSubmissionPollState `json:"poll_state" gorm:"type:varchar(32);index;index:idx_task_submission_poll_updated,priority:1"`
	SendAttempts          int                     `json:"send_attempts" gorm:"not null"`

	PreparedAt     int64  `json:"prepared_at" gorm:"bigint;index"`
	SendStartedAt  *int64 `json:"send_started_at,omitempty" gorm:"bigint"`
	SendDeadlineAt *int64 `json:"send_deadline_at,omitempty" gorm:"bigint;index"`
	ResolvedAt     *int64 `json:"resolved_at,omitempty" gorm:"bigint"`
	ConfirmedAt    *int64 `json:"confirmed_at,omitempty" gorm:"bigint"`
	ReconciledAt   *int64 `json:"reconciled_at,omitempty" gorm:"bigint"`

	ChannelID             int     `json:"channel_id" gorm:"index;uniqueIndex:idx_task_submission_upstream,priority:1"`
	ChannelType           int     `json:"channel_type" gorm:"index"`
	CanonicalBaseOrigin   string  `json:"-" gorm:"type:varchar(255)"`
	Provider              string  `json:"provider" gorm:"type:varchar(32);index"`
	LogicalAccountID      string  `json:"-" gorm:"type:varchar(128);index"`
	CredentialRef         string  `json:"-" gorm:"type:varchar(255)"`
	CredentialFingerprint string  `json:"-" gorm:"type:char(64)"`
	OriginModelName       string  `json:"origin_model_name" gorm:"type:varchar(191);index"`
	UpstreamModelName     string  `json:"-" gorm:"type:varchar(191)"`
	UpstreamTaskID        *string `json:"-" gorm:"type:varchar(191);uniqueIndex:idx_task_submission_upstream,priority:2"`
	TaskDBID              *int64  `json:"-" gorm:"uniqueIndex"`

	ReservedQuota        int                        `json:"reserved_quota" gorm:"not null"`
	TokenID              *int                       `json:"token_id,omitempty" gorm:"index"`
	SubscriptionID       *int                       `json:"subscription_id,omitempty" gorm:"index"`
	FundingReservedQuota int                        `json:"funding_reserved_quota" gorm:"not null"`
	TokenReservedQuota   int                        `json:"token_reserved_quota" gorm:"not null"`
	BillingState         TaskSubmissionBillingState `json:"billing_state" gorm:"type:varchar(32);index;index:idx_task_submission_billing_updated,priority:1"`
	BillingSource        string                     `json:"billing_source" gorm:"type:varchar(32)"`
	BillingRequestID     string                     `json:"-" gorm:"type:varchar(128);uniqueIndex"`
	PublicPriceSnapshot  string                     `json:"-" gorm:"type:text"`

	ProviderCostState               TaskSubmissionProviderCostState `json:"provider_cost_state" gorm:"type:varchar(32);index;index:idx_task_submission_cost_updated,priority:1"`
	ProviderCostSnapshot            string                          `json:"-" gorm:"type:text"`
	ProviderUsageTotalTokens        *int64                          `json:"-" gorm:"bigint"`
	ProviderCostMicrounits          *int64                          `json:"-" gorm:"bigint"`
	ProviderCostCurrency            string                          `json:"-" gorm:"type:varchar(8)"`
	ProviderCostPotentialMicrounits int64                           `json:"-" gorm:"bigint;not null;default:0"`

	ProviderStatus        string `json:"provider_status,omitempty" gorm:"type:varchar(64)"`
	SafeErrorCode         string `json:"error_code,omitempty" gorm:"type:varchar(128);index"`
	SafeErrorMessage      string `json:"error_message,omitempty" gorm:"type:varchar(512)"`
	ProviderHTTPStatus    *int   `json:"-"`
	ProviderRequestID     string `json:"-" gorm:"type:varchar(191);index"`
	ResponseDigest        string `json:"-" gorm:"type:char(64)"`
	ResultSnapshotVersion int    `json:"-" gorm:"not null"`

	PollAttempts int                        `json:"poll_attempts" gorm:"not null"`
	LastPollAt   *int64                     `json:"last_poll_at,omitempty" gorm:"bigint"`
	NextPollAt   *int64                     `json:"next_poll_at,omitempty" gorm:"bigint;index"`
	ArchiveState TaskSubmissionArchiveState `json:"archive_state" gorm:"type:varchar(32);index"`
	// The complete immutable archive descriptor is frozen atomically with
	// settlement. None of these fields is a public download URL; the ref is an
	// opaque locator that is usable only through the private archive backend.
	ArchiveSchemaVersion      int    `json:"-" gorm:"column:archive_schema_version;not null;default:0"`
	ArchivedResultRef         string `json:"-" gorm:"column:archived_result_ref;type:varchar(512)"`
	ArchivedResultVersionID   string `json:"-" gorm:"column:archived_result_version_id;type:varchar(1024)"`
	ArchivedResultSHA256      string `json:"-" gorm:"column:archived_result_sha256;type:char(64)"`
	ArchivedResultSize        int64  `json:"-" gorm:"column:archived_result_size;bigint;not null;default:0"`
	ArchivedResultContentType string `json:"-" gorm:"column:archived_result_content_type;type:varchar(64)"`
	ArchivedResultETag        string `json:"-" gorm:"column:archived_result_etag;type:varchar(512)"`

	ReconciledBy  string `json:"-" gorm:"type:varchar(128)"`
	ReconcileNote string `json:"-" gorm:"type:varchar(512)"`
}

func (s *TaskSubmission) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if s.CreatedAt == 0 {
		s.CreatedAt = now
	}
	if s.UpdatedAt == 0 {
		s.UpdatedAt = now
	}
	if s.PreparedAt == 0 {
		s.PreparedAt = now
	}
	if s.Version == 0 {
		s.Version = 1
	}
	if s.PollState == "" {
		s.PollState = TaskSubmissionPollStateNone
	}
	if s.ArchiveState == "" {
		s.ArchiveState = TaskSubmissionArchiveStateNone
	}
	if s.ResultSnapshotVersion == 0 {
		s.ResultSnapshotVersion = 1
	}
	return nil
}

// TaskSubmissionBillingEntry is append-only. State transitions and the exact
// authoritative balances observed in their transaction are preserved here;
// history must never be reconstructed from current pricing or balances.
type TaskSubmissionBillingEntry struct {
	ID                      int64                          `json:"id" gorm:"primaryKey"`
	SubmissionID            int64                          `json:"submission_id" gorm:"index"`
	BillingRequestID        string                         `json:"billing_request_id" gorm:"type:varchar(128);uniqueIndex:idx_task_submission_billing_operation,priority:1"`
	Operation               TaskSubmissionBillingOperation `json:"operation" gorm:"type:varchar(32);uniqueIndex:idx_task_submission_billing_operation,priority:2"`
	Sequence                int                            `json:"sequence" gorm:"uniqueIndex:idx_task_submission_billing_operation,priority:3"`
	UserID                  int                            `json:"user_id" gorm:"index"`
	TokenID                 *int                           `json:"token_id,omitempty" gorm:"index"`
	SubscriptionID          *int                           `json:"subscription_id,omitempty" gorm:"index"`
	FundingSource           string                         `json:"funding_source" gorm:"type:varchar(32)"`
	FundingQuota            int                            `json:"funding_quota" gorm:"not null"`
	TokenQuota              int                            `json:"token_quota" gorm:"not null"`
	BillingStateBefore      TaskSubmissionBillingState     `json:"billing_state_before" gorm:"type:varchar(32)"`
	BillingStateAfter       TaskSubmissionBillingState     `json:"billing_state_after" gorm:"type:varchar(32)"`
	UserQuotaBefore         *int                           `json:"user_quota_before,omitempty"`
	UserQuotaAfter          *int                           `json:"user_quota_after,omitempty"`
	TokenQuotaBefore        *int                           `json:"token_quota_before,omitempty"`
	TokenQuotaAfter         *int                           `json:"token_quota_after,omitempty"`
	TokenUsedBefore         *int                           `json:"token_used_before,omitempty"`
	TokenUsedAfter          *int                           `json:"token_used_after,omitempty"`
	SubscriptionUsedBefore  *int64                         `json:"subscription_used_before,omitempty" gorm:"bigint"`
	SubscriptionUsedAfter   *int64                         `json:"subscription_used_after,omitempty" gorm:"bigint"`
	SubmissionVersionBefore int64                          `json:"submission_version_before" gorm:"bigint"`
	SubmissionVersionAfter  int64                          `json:"submission_version_after" gorm:"bigint"`
	ReasonCode              string                         `json:"reason_code,omitempty" gorm:"type:varchar(128)"`
	Actor                   string                         `json:"actor,omitempty" gorm:"type:varchar(128)"`
	CreatedAt               int64                          `json:"created_at" gorm:"bigint;index"`
}

func (e *TaskSubmissionBillingEntry) BeforeCreate(_ *gorm.DB) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = common.GetTimestamp()
	}
	return nil
}

func (*TaskSubmissionBillingEntry) BeforeUpdate(_ *gorm.DB) error {
	return ErrTaskSubmissionBillingEntryImmutable
}

// QuotaCacheInvalidationOutbox makes cache invalidation recoverable after the
// authoritative balance transaction commits. Consumers are idempotent; a
// lease only reduces duplicate work and is not a correctness boundary.
type QuotaCacheInvalidationOutbox struct {
	ID             int64  `json:"id" gorm:"primaryKey"`
	EventKey       string `json:"event_key" gorm:"type:varchar(191);uniqueIndex"`
	AggregateType  string `json:"aggregate_type" gorm:"type:varchar(32);index"`
	AggregateID    int64  `json:"aggregate_id" gorm:"bigint;index"`
	BalanceVersion int64  `json:"balance_version" gorm:"bigint;not null"`
	Attempts       int    `json:"attempts" gorm:"not null"`
	AvailableAt    int64  `json:"available_at" gorm:"bigint;index"`
	LockedBy       string `json:"locked_by" gorm:"type:varchar(128);index"`
	LockedUntil    int64  `json:"locked_until" gorm:"bigint;index"`
	ProcessedAt    *int64 `json:"processed_at,omitempty" gorm:"bigint;index"`
	LastError      string `json:"last_error,omitempty" gorm:"type:varchar(512)"`
	CreatedAt      int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt      int64  `json:"updated_at" gorm:"bigint;index"`
}

func (e *QuotaCacheInvalidationOutbox) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	if e.UpdatedAt == 0 {
		e.UpdatedAt = now
	}
	if e.AvailableAt == 0 {
		e.AvailableAt = now
	}
	return nil
}

func GetTaskSubmissionByUserClientRequestID(userID int, clientRequestID string) (*TaskSubmission, error) {
	if userID <= 0 || strings.TrimSpace(clientRequestID) == "" {
		return nil, nil
	}
	var submission TaskSubmission
	result := DB.Where("user_id = ? AND client_request_id = ?", userID, clientRequestID).Limit(1).Find(&submission)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &submission, nil
}

func GetTaskSubmissionByID(id int64) (*TaskSubmission, error) {
	if id <= 0 {
		return nil, nil
	}
	var submission TaskSubmission
	result := DB.Where("id = ?", id).Limit(1).Find(&submission)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &submission, nil
}

func GetTaskSubmissionByPublicTaskID(publicTaskID string) (*TaskSubmission, error) {
	if strings.TrimSpace(publicTaskID) == "" {
		return nil, nil
	}
	var submission TaskSubmission
	result := DB.Where("public_task_id = ?", publicTaskID).Limit(1).Find(&submission)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &submission, nil
}

func GetTaskSubmissionByTaskDBID(taskDBID int64) (*TaskSubmission, error) {
	if taskDBID <= 0 {
		return nil, nil
	}
	var submission TaskSubmission
	result := DB.Where("task_db_id = ?", taskDBID).Limit(1).Find(&submission)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &submission, nil
}

type TaskSubmissionListFilter struct {
	UserID            int
	ClientRequestID   string
	PublicTaskID      string
	ProviderRequestID string
	UpstreamTaskID    string
	Provider          string
	ChannelID         int
	State             TaskSubmissionState
	ErrorCode         string
	CreatedAfter      int64
	CreatedBefore     int64
	Limit             int
	Offset            int
}

func ListTaskSubmissions(filter TaskSubmissionListFilter) ([]*TaskSubmission, error) {
	query := applyTaskSubmissionListFilter(DB.Model(&TaskSubmission{}), filter)
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var submissions []*TaskSubmission
	err := query.Order("id desc").Limit(limit).Offset(filter.Offset).Find(&submissions).Error
	return submissions, err
}

func CountTaskSubmissions(filter TaskSubmissionListFilter) (int64, error) {
	var count int64
	err := applyTaskSubmissionListFilter(DB.Model(&TaskSubmission{}), filter).Count(&count).Error
	return count, err
}

func applyTaskSubmissionListFilter(query *gorm.DB, filter TaskSubmissionListFilter) *gorm.DB {
	if filter.UserID > 0 {
		query = query.Where("user_id = ?", filter.UserID)
	}
	if filter.ClientRequestID != "" {
		query = query.Where("client_request_id = ?", filter.ClientRequestID)
	}
	if filter.PublicTaskID != "" {
		query = query.Where("public_task_id = ?", filter.PublicTaskID)
	}
	if filter.ProviderRequestID != "" {
		query = query.Where("provider_request_id = ?", filter.ProviderRequestID)
	}
	if filter.UpstreamTaskID != "" {
		query = query.Where("upstream_task_id = ?", filter.UpstreamTaskID)
	}
	if filter.Provider != "" {
		query = query.Where("provider = ?", filter.Provider)
	}
	if filter.ChannelID > 0 {
		query = query.Where("channel_id = ?", filter.ChannelID)
	}
	if filter.State != "" {
		query = query.Where("state = ?", filter.State)
	}
	if filter.ErrorCode != "" {
		query = query.Where("safe_error_code = ?", filter.ErrorCode)
	}
	if filter.CreatedAfter > 0 {
		query = query.Where("created_at >= ?", filter.CreatedAfter)
	}
	if filter.CreatedBefore > 0 {
		query = query.Where("created_at <= ?", filter.CreatedBefore)
	}
	return query
}

// CASTaskSubmissionPollState applies a poll-only update without overwriting a
// concurrent terminal/archive/reconciliation result. Pass an empty expected
// poll state to guard only on ID + version.
func CASTaskSubmissionPollState(id int64, expectedVersion int64, expectedPollState TaskSubmissionPollState, newPollState TaskSubmissionPollState, nextPollAt *int64, safeErrorCode string) (bool, error) {
	if !validTaskSubmissionPollState(newPollState) || len(safeErrorCode) > 128 {
		return false, errors.New("invalid task submission poll state mutation")
	}
	query := DB.Model(&TaskSubmission{}).Where("id = ? AND version = ? AND state = ?", id, expectedVersion, TaskSubmissionStateConfirmed)
	if expectedPollState != "" {
		query = query.Where("poll_state = ?", expectedPollState)
	}
	updates := map[string]any{
		"poll_state":      newPollState,
		"next_poll_at":    nextPollAt,
		"safe_error_code": safeErrorCode,
		"updated_at":      common.GetTimestamp(),
		"version":         gorm.Expr("version + ?", 1),
	}
	result := query.Updates(updates)
	return result.RowsAffected == 1, result.Error
}

func validTaskSubmissionPollState(state TaskSubmissionPollState) bool {
	switch state {
	case TaskSubmissionPollStateNone,
		TaskSubmissionPollStateActive,
		TaskSubmissionPollStateBackoff,
		TaskSubmissionPollStatePausedCredential,
		TaskSubmissionPollStateStale,
		TaskSubmissionPollStateTerminal:
		return true
	default:
		return false
	}
}

func FindStaleSendingTaskSubmissions(deadline int64, limit int) ([]*TaskSubmission, error) {
	if limit <= 0 {
		limit = 100
	}
	var submissions []*TaskSubmission
	err := DB.Where("state = ? AND send_attempts = ? AND send_deadline_at IS NOT NULL AND send_deadline_at <= ?", TaskSubmissionStateSending, 1, deadline).
		Order("send_deadline_at asc").Limit(limit).Find(&submissions).Error
	return submissions, err
}

func FindStalePreparedTaskSubmissions(preparedBefore int64, limit int) ([]*TaskSubmission, error) {
	if limit <= 0 {
		limit = 100
	}
	var submissions []*TaskSubmission
	err := DB.Where("state = ? AND send_attempts = ? AND billing_state = ? AND prepared_at <= ?", TaskSubmissionStatePrepared, 0, TaskSubmissionBillingStateReserved, preparedBefore).
		Order("prepared_at asc").Limit(limit).Find(&submissions).Error
	return submissions, err
}

func ClaimQuotaCacheInvalidationOutbox(limit int, workerID string, leaseUntil int64) ([]*QuotaCacheInvalidationOutbox, error) {
	if limit <= 0 {
		limit = 100
	}
	now := common.GetTimestamp()
	var candidates []*QuotaCacheInvalidationOutbox
	if err := DB.Where("processed_at IS NULL AND available_at <= ? AND (locked_until = 0 OR locked_until < ?)", now, now).
		Order("id asc").Limit(limit).Find(&candidates).Error; err != nil {
		return nil, err
	}
	claimed := make([]*QuotaCacheInvalidationOutbox, 0, len(candidates))
	for _, candidate := range candidates {
		result := DB.Model(&QuotaCacheInvalidationOutbox{}).
			Where("id = ? AND processed_at IS NULL AND available_at <= ? AND (locked_until = 0 OR locked_until < ?)", candidate.ID, now, now).
			Updates(map[string]any{
				"locked_by":    workerID,
				"locked_until": leaseUntil,
				"attempts":     gorm.Expr("attempts + ?", 1),
				"updated_at":   now,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			continue
		}
		candidate.LockedBy = workerID
		candidate.LockedUntil = leaseUntil
		candidate.Attempts++
		claimed = append(claimed, candidate)
	}
	return claimed, nil
}

func CompleteQuotaCacheInvalidationOutbox(id int64, workerID string) error {
	now := common.GetTimestamp()
	result := DB.Model(&QuotaCacheInvalidationOutbox{}).
		Where("id = ? AND processed_at IS NULL AND locked_by = ?", id, workerID).
		Updates(map[string]any{
			"processed_at": now,
			"locked_by":    "",
			"locked_until": 0,
			"last_error":   "",
			"updated_at":   now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func ReleaseQuotaCacheInvalidationOutbox(id int64, workerID string, availableAt int64, safeError string) error {
	if len(safeError) > 512 {
		safeError = safeError[:512]
	}
	result := DB.Model(&QuotaCacheInvalidationOutbox{}).
		Where("id = ? AND processed_at IS NULL AND locked_by = ?", id, workerID).
		Updates(map[string]any{
			"available_at": availableAt,
			"locked_by":    "",
			"locked_until": 0,
			"last_error":   safeError,
			"updated_at":   common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
