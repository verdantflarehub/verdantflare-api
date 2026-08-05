package service

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrTaskSubmissionNotFound            = errors.New("task submission not found")
	ErrTaskSubmissionIdempotencyConflict = errors.New("task submission idempotency conflict")
	ErrTaskSubmissionCASLost             = errors.New("task submission compare-and-swap lost")
	ErrTaskSubmissionInvalidTransition   = errors.New("invalid task submission transition")
	ErrTaskSubmissionInsufficientQuota   = errors.New("insufficient quota for task submission")
	ErrTaskSubmissionBalanceReconcile    = errors.New("task submission balance requires reconciliation")
	ErrTaskSubmissionBatchQuotaUnsafe    = errors.New("task submission billing requires database-authoritative quota; disable batch quota updates")
)

type SubmissionRequestSummary struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	Duration      int    `json:"duration"`
	Ratio         string `json:"ratio"`
	Resolution    string `json:"resolution"`
	ImageCount    int    `json:"image_count"`
	VideoCount    int    `json:"video_count"`
	AudioCount    int    `json:"audio_count"`
	GenerateAudio bool   `json:"generate_audio"`
	Watermark     bool   `json:"watermark"`
	Seed          int    `json:"seed"`
}

type SubmissionPublicPriceSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	PriceVersion  string `json:"price_version"`
	ModelName     string `json:"model_name"`
	ProductQuota  int    `json:"product_quota"`
	GroupRatio    string `json:"group_ratio"`
	Currency      string `json:"currency"`
}

type SubmissionProviderCostSnapshot struct {
	SchemaVersion          int    `json:"schema_version"`
	ProviderPriceKey       string `json:"provider_price_key"`
	ProviderPriceVersion   string `json:"provider_price_version"`
	Provider               string `json:"provider"`
	UpstreamModelName      string `json:"upstream_model_name"`
	Resolution             string `json:"resolution"`
	HasVideo               bool   `json:"has_video"`
	Duration               int    `json:"duration"`
	ImageCount             int    `json:"image_count"`
	VideoCount             int    `json:"video_count"`
	AudioCount             int    `json:"audio_count"`
	UnitPricePerMillionCNY string `json:"unit_price_per_million_cny"`
	RoundingRule           string `json:"rounding_rule"`
}

type PrepareTaskSubmissionInput struct {
	UserID          int
	ClientRequestID string
	RequestDigest   string
	DigestVersion   string
	RequestSummary  SubmissionRequestSummary

	ProviderRequestDigest string
	PublicTaskID          string
	ChannelID             int
	ChannelType           int
	CanonicalBaseOrigin   string
	Provider              string
	LogicalAccountID      string
	CredentialRef         string
	CredentialFingerprint string
	OriginModelName       string
	UpstreamModelName     string

	ReservedQuota        int
	TokenID              int
	FundingSource        string
	SubscriptionID       int
	PublicPriceSnapshot  SubmissionPublicPriceSnapshot
	ProviderCostSnapshot SubmissionProviderCostSnapshot
}

type TaskSubmissionUserView struct {
	ClientRequestID string                           `json:"client_request_id"`
	SubmissionState model.TaskSubmissionState        `json:"submission_state"`
	PublicTaskID    string                           `json:"public_task_id,omitempty"`
	BillingState    model.TaskSubmissionBillingState `json:"billing_state"`
	ErrorCode       string                           `json:"error_code,omitempty"`
	ErrorMessage    string                           `json:"error_message,omitempty"`
	CreatedAt       int64                            `json:"created_at"`
	UpdatedAt       int64                            `json:"updated_at"`
}

type submissionBalanceSnapshot struct {
	UserQuotaBefore        *int
	UserQuotaAfter         *int
	TokenQuotaBefore       *int
	TokenQuotaAfter        *int
	TokenUsedBefore        *int
	TokenUsedAfter         *int
	SubscriptionUsedBefore *int64
	SubscriptionUsedAfter  *int64
	FundingReservedQuota   int
	TokenReservedQuota     int
}

func LookupTaskSubmission(userID int, clientRequestID string, requestDigest string) (*model.TaskSubmission, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(clientRequestID))
	if err != nil {
		return nil, fmt.Errorf("invalid client request id: %w", err)
	}
	submission, err := model.GetTaskSubmissionByUserClientRequestID(userID, parsed.String())
	if err != nil || submission == nil {
		return submission, err
	}
	if !strings.EqualFold(submission.RequestDigest, requestDigest) {
		return nil, ErrTaskSubmissionIdempotencyConflict
	}
	return submission, nil
}

// PrepareTaskSubmission performs T0/T1. A returned created=false is an exact
// replay of an existing intent and must not be priced, routed or submitted
// again by the caller.
func PrepareTaskSubmission(input PrepareTaskSubmissionInput) (*model.TaskSubmission, bool, error) {
	normalizedIdentity, err := normalizeTaskSubmissionIdentity(input)
	if err != nil {
		return nil, false, err
	}

	// T0 intentionally precedes current pricing/channel/deployment validation.
	// An existing intent must remain recoverable after configuration changes.
	existing, err := model.GetTaskSubmissionByUserClientRequestID(normalizedIdentity.UserID, normalizedIdentity.ClientRequestID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !strings.EqualFold(existing.RequestDigest, normalizedIdentity.RequestDigest) {
			return nil, false, ErrTaskSubmissionIdempotencyConflict
		}
		return existing, false, nil
	}

	// Existing batch quota updates are process-local and make database balances
	// temporarily stale. They cannot participate in the T1 row locks or the
	// durable outbox, especially across multiple nodes, so proceeding would
	// permit overspend. Fail closed until the deployment uses authoritative
	// per-request DB balance updates.
	if common.BatchUpdateEnabled {
		return nil, false, ErrTaskSubmissionBatchQuotaUnsafe
	}
	normalized, err := normalizePrepareTaskSubmissionInput(normalizedIdentity)
	if err != nil {
		return nil, false, err
	}

	requestSummary, err := marshalSubmissionSnapshot(normalized.RequestSummary)
	if err != nil {
		return nil, false, err
	}
	publicPriceSnapshot, err := marshalSubmissionSnapshot(normalized.PublicPriceSnapshot)
	if err != nil {
		return nil, false, err
	}
	providerCostSnapshot, err := marshalSubmissionSnapshot(normalized.ProviderCostSnapshot)
	if err != nil {
		return nil, false, err
	}
	publicTaskID := normalized.PublicTaskID
	if publicTaskID == "" {
		publicTaskID = model.GenerateTaskID()
	}
	tokenID := nullablePositiveInt(normalized.TokenID)
	subscriptionID := nullablePositiveInt(normalized.SubscriptionID)
	submission := &model.TaskSubmission{
		UserID:                normalized.UserID,
		ClientRequestID:       normalized.ClientRequestID,
		RequestDigest:         strings.ToLower(normalized.RequestDigest),
		DigestVersion:         normalized.DigestVersion,
		RequestSummary:        requestSummary,
		ProviderRequestDigest: strings.ToLower(normalized.ProviderRequestDigest),
		PublicTaskID:          publicTaskID,
		State:                 model.TaskSubmissionStatePrepared,
		PollState:             model.TaskSubmissionPollStateNone,
		ChannelID:             normalized.ChannelID,
		ChannelType:           normalized.ChannelType,
		CanonicalBaseOrigin:   normalized.CanonicalBaseOrigin,
		Provider:              normalized.Provider,
		LogicalAccountID:      normalized.LogicalAccountID,
		CredentialRef:         normalized.CredentialRef,
		CredentialFingerprint: strings.ToLower(normalized.CredentialFingerprint),
		OriginModelName:       normalized.OriginModelName,
		UpstreamModelName:     normalized.UpstreamModelName,
		ReservedQuota:         normalized.ReservedQuota,
		TokenID:               tokenID,
		SubscriptionID:        subscriptionID,
		BillingState:          model.TaskSubmissionBillingStateNone,
		BillingSource:         normalized.FundingSource,
		BillingRequestID:      fmt.Sprintf("submission:%d:%s", normalized.UserID, normalized.ClientRequestID),
		PublicPriceSnapshot:   publicPriceSnapshot,
		ProviderCostState:     model.TaskSubmissionProviderCostNotApplicable,
		ProviderCostSnapshot:  providerCostSnapshot,
		ArchiveState:          model.TaskSubmissionArchiveStateNone,
		ResultSnapshotVersion: 1,
	}

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		return CreatePreparedSubmissionTx(tx, submission)
	})
	if err == nil {
		return submission, true, nil
	}

	// A concurrent insert is a safe T0 replay. Its transaction owns the only
	// reservation; this transaction has already rolled back before the read.
	winner, lookupErr := model.GetTaskSubmissionByUserClientRequestID(normalized.UserID, normalized.ClientRequestID)
	if lookupErr != nil {
		return nil, false, errors.Join(err, lookupErr)
	}
	if winner != nil {
		if !strings.EqualFold(winner.RequestDigest, normalized.RequestDigest) {
			return nil, false, ErrTaskSubmissionIdempotencyConflict
		}
		return winner, false, nil
	}
	return nil, false, err
}

// CreatePreparedSubmissionTx inserts the intent before touching balances, then
// reserves exact authoritative balances in the same transaction. Callers may
// safely retry the transaction before T2 because it has no provider side
// effect.
func CreatePreparedSubmissionTx(tx *gorm.DB, submission *model.TaskSubmission) error {
	if tx == nil || submission == nil {
		return errors.New("nil task submission transaction")
	}
	if submission.State != model.TaskSubmissionStatePrepared || submission.BillingState != model.TaskSubmissionBillingStateNone {
		return ErrTaskSubmissionInvalidTransition
	}
	if err := reserveWxmaasChannelSafetyTx(tx, submission); err != nil {
		return err
	}
	if err := tx.Create(submission).Error; err != nil {
		return err
	}
	return ReserveSubmissionTx(tx, submission)
}

// ReserveSubmissionTx is the T1 quota primitive. It never consults quota
// caches or batch updaters: rows locked in this transaction are authoritative.
func ReserveSubmissionTx(tx *gorm.DB, submission *model.TaskSubmission) error {
	if tx == nil || submission == nil || submission.ID <= 0 {
		return errors.New("invalid task submission reservation")
	}
	if submission.ReservedQuota <= 0 || submission.ReservedQuota > common.MaxQuota {
		return fmt.Errorf("invalid reserved quota: %d", submission.ReservedQuota)
	}
	balances, err := reserveSubmissionBalancesTx(tx, submission)
	if err != nil {
		return err
	}

	versionBefore := submission.Version
	versionAfter := versionBefore + 1
	now := common.GetTimestamp()
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND billing_state = ?", submission.ID, versionBefore, model.TaskSubmissionStatePrepared, model.TaskSubmissionBillingStateNone).
		Updates(map[string]any{
			"funding_reserved_quota": balances.FundingReservedQuota,
			"token_reserved_quota":   balances.TokenReservedQuota,
			"billing_state":          model.TaskSubmissionBillingStateReserved,
			"updated_at":             now,
			"version":                versionAfter,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskSubmissionCASLost
	}

	entry := &model.TaskSubmissionBillingEntry{
		SubmissionID:            submission.ID,
		BillingRequestID:        submission.BillingRequestID,
		Operation:               model.TaskSubmissionBillingOperationReserve,
		Sequence:                1,
		UserID:                  submission.UserID,
		TokenID:                 submission.TokenID,
		SubscriptionID:          submission.SubscriptionID,
		FundingSource:           submission.BillingSource,
		FundingQuota:            balances.FundingReservedQuota,
		TokenQuota:              balances.TokenReservedQuota,
		BillingStateBefore:      model.TaskSubmissionBillingStateNone,
		BillingStateAfter:       model.TaskSubmissionBillingStateReserved,
		UserQuotaBefore:         balances.UserQuotaBefore,
		UserQuotaAfter:          balances.UserQuotaAfter,
		TokenQuotaBefore:        balances.TokenQuotaBefore,
		TokenQuotaAfter:         balances.TokenQuotaAfter,
		TokenUsedBefore:         balances.TokenUsedBefore,
		TokenUsedAfter:          balances.TokenUsedAfter,
		SubscriptionUsedBefore:  balances.SubscriptionUsedBefore,
		SubscriptionUsedAfter:   balances.SubscriptionUsedAfter,
		SubmissionVersionBefore: versionBefore,
		SubmissionVersionAfter:  versionAfter,
		ReasonCode:              "submission_prepared",
		Actor:                   "system",
	}
	if err := tx.Create(entry).Error; err != nil {
		return err
	}
	if err := enqueueSubmissionQuotaInvalidationTx(tx, entry); err != nil {
		return err
	}

	submission.Version = versionAfter
	submission.UpdatedAt = now
	submission.BillingState = model.TaskSubmissionBillingStateReserved
	submission.FundingReservedQuota = balances.FundingReservedQuota
	submission.TokenReservedQuota = balances.TokenReservedQuota
	return nil
}

func AcquireSubmissionSend(submissionID int64, expectedVersion int64, sendDeadlineAt int64) (*model.TaskSubmission, bool, error) {
	var claimed *model.TaskSubmission
	var won bool
	var claimErr error
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		claimed, won, err = AcquireSubmissionSendTx(tx, submissionID, expectedVersion, sendDeadlineAt)
		if isSD2ChannelSafetyError(err) {
			if _, rejectErr := RejectSubmissionTx(tx, submissionID, expectedVersion, model.TaskSubmissionStatePrepared, "channel_safety_gate_rejected", "system"); rejectErr != nil {
				return rejectErr
			}
			claimErr = err
			return nil
		}
		return err
	})
	if err == nil && claimErr != nil {
		return nil, false, claimErr
	}
	return claimed, won, err
}

// AcquireSubmissionSendTx is T2. Only RowsAffected=1 grants permission to send
// provider bytes. send_attempts can never be reset by this API.
func AcquireSubmissionSendTx(tx *gorm.DB, submissionID int64, expectedVersion int64, sendDeadlineAt int64) (*model.TaskSubmission, bool, error) {
	if tx == nil || submissionID <= 0 || expectedVersion <= 0 {
		return nil, false, errors.New("invalid task submission send claim")
	}
	now := common.GetTimestamp()
	if sendDeadlineAt <= now {
		return nil, false, errors.New("send deadline must be in the future")
	}
	var current model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, ErrTaskSubmissionNotFound
		}
		return nil, false, err
	}
	if current.Version != expectedVersion || current.State != model.TaskSubmissionStatePrepared || current.BillingState != model.TaskSubmissionBillingStateReserved || current.SendAttempts != 0 {
		return nil, false, nil
	}
	if err := checkWxmaasChannelSafetyBeforeSendTx(tx, &current); err != nil {
		return nil, false, err
	}
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND billing_state = ? AND send_attempts = ?", submissionID, expectedVersion, model.TaskSubmissionStatePrepared, model.TaskSubmissionBillingStateReserved, 0).
		Updates(map[string]any{
			"state":               model.TaskSubmissionStateSending,
			"send_attempts":       1,
			"send_started_at":     now,
			"send_deadline_at":    sendDeadlineAt,
			"provider_cost_state": model.TaskSubmissionProviderCostPending,
			"updated_at":          now,
			"version":             expectedVersion + 1,
		})
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, false, nil
	}
	var submission model.TaskSubmission
	if err := tx.First(&submission, submissionID).Error; err != nil {
		return nil, false, err
	}
	return &submission, true, nil
}

func isSD2ChannelSafetyError(err error) bool {
	return errors.Is(err, ErrSD2ChannelSafetyConfig) ||
		errors.Is(err, ErrSD2ChannelCreateDisabled) ||
		errors.Is(err, ErrSD2ChannelConcurrencyLimit) ||
		errors.Is(err, ErrSD2ChannelDailyBudget)
}

func MarkSubmissionUnknownTx(tx *gorm.DB, submissionID int64, expectedVersion int64, safeErrorCode string, safeErrorMessage string, providerHTTPStatus *int, responseDigest string) (*model.TaskSubmission, error) {
	if tx == nil {
		return nil, errors.New("nil transaction")
	}
	if responseDigest != "" && !validSHA256Hex(responseDigest) {
		return nil, errors.New("invalid response digest")
	}
	safeErrorCode = truncateSafeText(safeErrorCode, 128)
	safeErrorMessage = sanitizeSubmissionErrorMessage(safeErrorMessage)
	now := common.GetTimestamp()
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND send_attempts = ? AND billing_state = ?", submissionID, expectedVersion, model.TaskSubmissionStateSending, 1, model.TaskSubmissionBillingStateReserved).
		Updates(map[string]any{
			"state":                model.TaskSubmissionStateUnknown,
			"safe_error_code":      safeErrorCode,
			"safe_error_message":   safeErrorMessage,
			"provider_http_status": providerHTTPStatus,
			"response_digest":      strings.ToLower(responseDigest),
			"resolved_at":          now,
			"updated_at":           now,
			"version":              expectedVersion + 1,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	var submission model.TaskSubmission
	if err := tx.First(&submission, submissionID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func MarkSubmissionUnknown(submissionID int64, expectedVersion int64, safeErrorCode string, safeErrorMessage string, providerHTTPStatus *int, responseDigest string) (*model.TaskSubmission, error) {
	var submission *model.TaskSubmission
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		submission, err = MarkSubmissionUnknownTx(tx, submissionID, expectedVersion, safeErrorCode, safeErrorMessage, providerHTTPStatus, responseDigest)
		return err
	})
	return submission, err
}

// SweepStaleSendingTaskSubmissions converts a crashed/abandoned T2 owner to
// UNKNOWN. It never retries provider creation and never refunds the reservation.
func SweepStaleSendingTaskSubmissions(limit int) (int, error) {
	submissions, err := model.FindStaleSendingTaskSubmissions(common.GetTimestamp(), limit)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, submission := range submissions {
		_, err := MarkSubmissionUnknown(submission.ID, submission.Version, "send_deadline_elapsed", "the provider submission outcome requires reconciliation", nil, "")
		if errors.Is(err, ErrTaskSubmissionCASLost) {
			continue
		}
		if err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// SweepStalePreparedTaskSubmissions rejects only intents that never acquired
// T2 send permission. Because send_attempts is still zero, refunding the exact
// frozen reservation is safe and cannot orphan a provider task.
func SweepStalePreparedTaskSubmissions(preparedBefore int64, limit int) (int, error) {
	submissions, err := model.FindStalePreparedTaskSubmissions(preparedBefore, limit)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, candidate := range submissions {
		err := model.DB.Transaction(func(tx *gorm.DB) error {
			_, err := RejectSubmissionTx(tx, candidate.ID, candidate.Version, model.TaskSubmissionStatePrepared, "prepared_intent_expired", "submission-recovery")
			return err
		})
		if errors.Is(err, ErrTaskSubmissionCASLost) {
			continue
		}
		if err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// CommitSubmissionTx is T3: the public Task, private provider ID and billing
// commitment become visible in one transaction. It is safe to retry this DB
// transaction with the same provider result; it never calls the provider.
func CommitSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, expectedState model.TaskSubmissionState, upstreamTaskID string, task *model.Task, actor string) (*model.TaskSubmission, error) {
	if tx == nil || task == nil {
		return nil, errors.New("invalid task submission commit")
	}
	if expectedState != model.TaskSubmissionStateSending && expectedState != model.TaskSubmissionStateUnknown {
		return nil, ErrTaskSubmissionInvalidTransition
	}
	upstreamTaskID = strings.TrimSpace(upstreamTaskID)
	if !validProviderTaskID(upstreamTaskID) {
		return nil, errors.New("invalid upstream task id")
	}
	var submission model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&submission).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskSubmissionNotFound
		}
		return nil, err
	}
	if submission.State == model.TaskSubmissionStateConfirmed && submission.UpstreamTaskID != nil && *submission.UpstreamTaskID == upstreamTaskID {
		return &submission, nil
	}
	if submission.Version != expectedVersion || submission.State != expectedState || submission.BillingState != model.TaskSubmissionBillingStateReserved || submission.TaskDBID != nil {
		return nil, ErrTaskSubmissionCASLost
	}

	var existingTask model.Task
	lookup := tx.Where("task_id = ?", submission.PublicTaskID).Limit(1).Find(&existingTask)
	if lookup.Error != nil {
		return nil, lookup.Error
	}
	if lookup.RowsAffected > 0 {
		return nil, ErrTaskSubmissionBalanceReconcile
	}

	task.ID = 0
	task.TaskID = submission.PublicTaskID
	task.UserId = submission.UserID
	task.ChannelId = submission.ChannelID
	task.Quota = submission.ReservedQuota
	task.PrivateData.Key = ""
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	task.PrivateData.ResultURL = ""
	task.PrivateData.BillingSource = submission.BillingSource
	if submission.SubscriptionID != nil {
		task.PrivateData.SubscriptionId = *submission.SubscriptionID
	}
	if submission.TokenID != nil {
		task.PrivateData.TokenId = *submission.TokenID
	}
	billingContext := task.PrivateData.BillingContext
	if billingContext == nil {
		billingContext = &model.TaskBillingContext{}
	}
	billingContext.OriginModelName = submission.OriginModelName
	billingContext.PerCallBilling = true
	task.PrivateData.BillingContext = billingContext
	if task.Status == "" || task.Status == model.TaskStatusNotStart {
		task.Status = model.TaskStatusSubmitted
	}
	if task.Progress == "" {
		task.Progress = "0%"
	}
	now := common.GetTimestamp()
	if task.CreatedAt == 0 {
		task.CreatedAt = now
	}
	if task.UpdatedAt == 0 {
		task.UpdatedAt = now
	}
	if task.SubmitTime == 0 {
		task.SubmitTime = now
	}
	if err := tx.Create(task).Error; err != nil {
		return nil, err
	}

	versionAfter := expectedVersion + 1
	updates := map[string]any{
		"state":              model.TaskSubmissionStateConfirmed,
		"poll_state":         model.TaskSubmissionPollStateActive,
		"upstream_task_id":   upstreamTaskID,
		"task_db_id":         task.ID,
		"billing_state":      model.TaskSubmissionBillingStateCommitted,
		"confirmed_at":       now,
		"resolved_at":        now,
		"safe_error_code":    "",
		"safe_error_message": "",
		"updated_at":         now,
		"version":            versionAfter,
	}
	if expectedState == model.TaskSubmissionStateUnknown {
		updates["reconciled_at"] = now
		updates["reconciled_by"] = truncateSafeText(actor, 128)
		updates["reconcile_note"] = "provider_task_confirmed"
	}
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND billing_state = ? AND task_db_id IS NULL", submission.ID, expectedVersion, expectedState, model.TaskSubmissionBillingStateReserved).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}

	entry := &model.TaskSubmissionBillingEntry{
		SubmissionID:            submission.ID,
		BillingRequestID:        submission.BillingRequestID,
		Operation:               model.TaskSubmissionBillingOperationCommit,
		Sequence:                1,
		UserID:                  submission.UserID,
		TokenID:                 submission.TokenID,
		SubscriptionID:          submission.SubscriptionID,
		FundingSource:           submission.BillingSource,
		FundingQuota:            submission.FundingReservedQuota,
		TokenQuota:              submission.TokenReservedQuota,
		BillingStateBefore:      model.TaskSubmissionBillingStateReserved,
		BillingStateAfter:       model.TaskSubmissionBillingStateCommitted,
		SubmissionVersionBefore: expectedVersion,
		SubmissionVersionAfter:  versionAfter,
		ReasonCode:              "provider_task_confirmed",
		Actor:                   truncateSafeText(actor, 128),
	}
	if entry.Actor == "" {
		entry.Actor = "system"
	}
	if err := tx.Create(entry).Error; err != nil {
		return nil, err
	}
	if err := tx.First(&submission, submission.ID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func CommitSubmission(submissionID int64, expectedVersion int64, expectedState model.TaskSubmissionState, upstreamTaskID string, task *model.Task, actor string) (*model.TaskSubmission, error) {
	var submission *model.TaskSubmission
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		submission, err = CommitSubmissionTx(tx, submissionID, expectedVersion, expectedState, upstreamTaskID, task, actor)
		return err
	})
	return submission, err
}

func ReconcileUnknownSubmissionConfirmed(submissionID int64, expectedVersion int64, upstreamTaskID string, task *model.Task, adminID int) (*model.TaskSubmission, error) {
	if adminID <= 0 {
		return nil, errors.New("admin id is required for reconciliation")
	}
	return CommitSubmission(submissionID, expectedVersion, model.TaskSubmissionStateUnknown, upstreamTaskID, task, fmt.Sprintf("admin:%d", adminID))
}

func ReconcileUnknownSubmissionRejected(submissionID int64, expectedVersion int64, reasonCode string, adminID int) (*model.TaskSubmission, error) {
	if adminID <= 0 {
		return nil, errors.New("admin id is required for reconciliation")
	}
	var submission *model.TaskSubmission
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		submission, err = RejectSubmissionTx(tx, submissionID, expectedVersion, model.TaskSubmissionStateUnknown, reasonCode, fmt.Sprintf("admin:%d", adminID))
		return err
	})
	return submission, err
}

func RejectSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, expectedState model.TaskSubmissionState, reasonCode string, actor string) (*model.TaskSubmission, error) {
	if expectedState != model.TaskSubmissionStateSending && expectedState != model.TaskSubmissionStateUnknown && expectedState != model.TaskSubmissionStatePrepared {
		return nil, ErrTaskSubmissionInvalidTransition
	}
	return refundSubmissionTx(tx, submissionID, expectedVersion, expectedState, model.TaskSubmissionStateRejected, model.TaskSubmissionBillingStateReserved, reasonCode, actor, true)
}

func RefundSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, expectedBillingState model.TaskSubmissionBillingState, reasonCode string, actor string) (*model.TaskSubmission, error) {
	return refundSubmissionTx(tx, submissionID, expectedVersion, "", "", expectedBillingState, reasonCode, actor, false)
}

func SettleSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, reasonCode string, actor string) (*model.TaskSubmission, error) {
	if tx == nil {
		return nil, errors.New("nil transaction")
	}
	var submission model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&submission).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskSubmissionNotFound
		}
		return nil, err
	}
	if submission.BillingState == model.TaskSubmissionBillingStateSettled {
		return &submission, nil
	}
	if submission.Version != expectedVersion || submission.State != model.TaskSubmissionStateConfirmed || submission.BillingState != model.TaskSubmissionBillingStateCommitted || submission.ArchiveState != model.TaskSubmissionArchiveStateArchived {
		return nil, ErrTaskSubmissionCASLost
	}
	versionAfter := expectedVersion + 1
	now := common.GetTimestamp()
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND billing_state = ? AND archive_state = ?", submission.ID, expectedVersion, model.TaskSubmissionStateConfirmed, model.TaskSubmissionBillingStateCommitted, model.TaskSubmissionArchiveStateArchived).
		Updates(map[string]any{
			"billing_state": model.TaskSubmissionBillingStateSettled,
			"poll_state":    model.TaskSubmissionPollStateTerminal,
			"updated_at":    now,
			"version":       versionAfter,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	entry := billingTransitionEntry(&submission, model.TaskSubmissionBillingOperationSettle, model.TaskSubmissionBillingStateCommitted, model.TaskSubmissionBillingStateSettled, expectedVersion, versionAfter, reasonCode, actor)
	if err := tx.Create(entry).Error; err != nil {
		return nil, err
	}
	if err := tx.First(&submission, submission.ID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func ArchiveAndSettleSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, archivedResult SD2ArchivedResult, actor string) (*model.TaskSubmission, error) {
	if tx == nil {
		return nil, errors.New("nil transaction")
	}
	if err := validateSD2ArchivedResult(archivedResult); err != nil {
		return nil, err
	}
	var current model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskSubmissionNotFound
		}
		return nil, err
	}
	if current.ArchiveState == model.TaskSubmissionArchiveStateArchived &&
		current.BillingState == model.TaskSubmissionBillingStateSettled &&
		current.PollState == model.TaskSubmissionPollStateTerminal {
		frozen, descriptorErr := SD2ArchivedResultFromSubmission(&current)
		if descriptorErr == nil && frozen == archivedResult {
			return &current, nil
		}
		return nil, ErrTaskSubmissionCASLost
	}
	if current.Version != expectedVersion || current.State != model.TaskSubmissionStateConfirmed || current.BillingState != model.TaskSubmissionBillingStateCommitted {
		return nil, ErrTaskSubmissionCASLost
	}
	providerCostUpdates, err := archivedSubmissionProviderCostUpdatesTx(tx, &current)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		"archive_state":                model.TaskSubmissionArchiveStateArchived,
		"archive_schema_version":       archivedResult.SchemaVersion,
		"archived_result_ref":          archivedResult.Ref,
		"archived_result_version_id":   archivedResult.VersionID,
		"archived_result_sha256":       archivedResult.SHA256,
		"archived_result_size":         archivedResult.Size,
		"archived_result_content_type": archivedResult.ContentType,
		"archived_result_etag":         archivedResult.ETag,
		"updated_at":                   common.GetTimestamp(),
		"version":                      expectedVersion + 1,
	}
	for field, value := range providerCostUpdates {
		updates[field] = value
	}
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ? AND billing_state = ? AND archive_state IN ?", submissionID, expectedVersion, model.TaskSubmissionStateConfirmed, model.TaskSubmissionBillingStateCommitted, []model.TaskSubmissionArchiveState{model.TaskSubmissionArchiveStateNone, model.TaskSubmissionArchiveStatePending, model.TaskSubmissionArchiveStateFailed}).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	return SettleSubmissionTx(tx, submissionID, expectedVersion+1, "result_archived", actor)
}

// SD2ArchivedResultFromSubmission reconstructs the frozen descriptor without
// consulting mutable object-store state. Callers must compare this complete
// value with the task snapshot before publishing a completed result.
func SD2ArchivedResultFromSubmission(submission *model.TaskSubmission) (SD2ArchivedResult, error) {
	if submission == nil || submission.ArchiveState != model.TaskSubmissionArchiveStateArchived {
		return SD2ArchivedResult{}, ErrInvalidSD2ArchivedResult
	}
	descriptor := SD2ArchivedResult{
		SchemaVersion: submission.ArchiveSchemaVersion,
		Ref:           submission.ArchivedResultRef,
		VersionID:     submission.ArchivedResultVersionID,
		SHA256:        submission.ArchivedResultSHA256,
		Size:          submission.ArchivedResultSize,
		ContentType:   submission.ArchivedResultContentType,
		ETag:          submission.ArchivedResultETag,
	}
	if err := validateSD2ArchivedResult(descriptor); err != nil {
		return SD2ArchivedResult{}, err
	}
	return descriptor, nil
}

func archivedSubmissionProviderCostUpdatesTx(tx *gorm.DB, submission *model.TaskSubmission) (map[string]any, error) {
	updates := map[string]any{}
	if submission.ProviderCostState == model.TaskSubmissionProviderCostRecorded || submission.ProviderCostState == model.TaskSubmissionProviderCostReconcileRequired {
		return updates, nil
	}
	markReconcile := func() (map[string]any, error) {
		return map[string]any{"provider_cost_state": model.TaskSubmissionProviderCostReconcileRequired}, nil
	}
	if submission.TaskDBID == nil || submission.ProviderCostSnapshot == "" {
		return markReconcile()
	}
	var task model.Task
	result := tx.Where("id = ?", *submission.TaskDBID).Limit(1).Find(&task)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return markReconcile()
	}
	taskSnapshot, err := DecodeSD2TaskResultSnapshot(task.Data)
	if err != nil || taskSnapshot.Usage.TotalTokens <= 0 {
		return markReconcile()
	}
	costMicrounits, ok := frozenProviderCostMicrounits(submission.ProviderCostSnapshot, taskSnapshot.Usage.TotalTokens)
	if !ok {
		return markReconcile()
	}
	return map[string]any{
		"provider_cost_state":         model.TaskSubmissionProviderCostRecorded,
		"provider_usage_total_tokens": taskSnapshot.Usage.TotalTokens,
		"provider_cost_microunits":    costMicrounits,
		"provider_cost_currency":      "CNY",
	}, nil
}

func RecordSubmissionProviderCostTx(tx *gorm.DB, submissionID int64, expectedVersion int64, totalTokens int64, costMicrounits int64, currency string) (*model.TaskSubmission, error) {
	if tx == nil || totalTokens <= 0 || costMicrounits <= 0 {
		return nil, errors.New("invalid provider cost")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != "CNY" {
		return nil, errors.New("unsupported provider cost currency")
	}
	var current model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&current).Error; err != nil {
		return nil, err
	}
	if current.Version != expectedVersion || current.ProviderCostState != model.TaskSubmissionProviderCostPending {
		return nil, ErrTaskSubmissionCASLost
	}
	expectedCostMicrounits, ok := frozenProviderCostMicrounits(current.ProviderCostSnapshot, totalTokens)
	if !ok || expectedCostMicrounits != costMicrounits {
		return nil, errors.New("provider cost does not match frozen price snapshot")
	}
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND provider_cost_state = ?", submissionID, expectedVersion, model.TaskSubmissionProviderCostPending).
		Updates(map[string]any{
			"provider_usage_total_tokens": totalTokens,
			"provider_cost_microunits":    costMicrounits,
			"provider_cost_currency":      currency,
			"provider_cost_state":         model.TaskSubmissionProviderCostRecorded,
			"updated_at":                  common.GetTimestamp(),
			"version":                     expectedVersion + 1,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	var submission model.TaskSubmission
	if err := tx.First(&submission, submissionID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func frozenProviderCostMicrounits(snapshotJSON string, totalTokens int64) (int64, bool) {
	if totalTokens <= 0 || strings.TrimSpace(snapshotJSON) == "" {
		return 0, false
	}
	var costSnapshot SubmissionProviderCostSnapshot
	if err := common.UnmarshalJsonStr(snapshotJSON, &costSnapshot); err != nil || costSnapshot.RoundingRule == "MANUAL_RECONCILE" {
		return 0, false
	}
	unitPrice, err := decimal.NewFromString(costSnapshot.UnitPricePerMillionCNY)
	if err != nil || !unitPrice.IsPositive() {
		return 0, false
	}
	// The frozen price is CNY per million tokens. One CNY is one million
	// microunits, so cost_microunits = total_tokens * unit_price. Keep the
	// calculation decimal and round once at the ledger boundary.
	costMicrounitsDecimal := decimal.NewFromInt(totalTokens).Mul(unitPrice).Round(0)
	if !costMicrounitsDecimal.IsPositive() || costMicrounitsDecimal.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, false
	}
	return costMicrounitsDecimal.IntPart(), true
}

func MarkSubmissionProviderCostReconcileTx(tx *gorm.DB, submissionID int64, expectedVersion int64, safeCode string) (*model.TaskSubmission, error) {
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND provider_cost_state IN ?", submissionID, expectedVersion, []model.TaskSubmissionProviderCostState{model.TaskSubmissionProviderCostPending, model.TaskSubmissionProviderCostRecorded}).
		Updates(map[string]any{
			"provider_cost_state": model.TaskSubmissionProviderCostReconcileRequired,
			"safe_error_code":     truncateSafeText(safeCode, 128),
			"updated_at":          common.GetTimestamp(),
			"version":             expectedVersion + 1,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	var submission model.TaskSubmission
	if err := tx.First(&submission, submissionID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func GetTaskSubmissionForUser(userID int, clientRequestID string) (*TaskSubmissionUserView, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(clientRequestID))
	if err != nil {
		return nil, nil
	}
	submission, err := model.GetTaskSubmissionByUserClientRequestID(userID, parsed.String())
	if err != nil || submission == nil {
		return nil, err
	}
	view := &TaskSubmissionUserView{
		ClientRequestID: submission.ClientRequestID,
		SubmissionState: submission.State,
		BillingState:    submission.BillingState,
		ErrorCode:       submission.SafeErrorCode,
		ErrorMessage:    submission.SafeErrorMessage,
		CreatedAt:       submission.CreatedAt,
		UpdatedAt:       submission.UpdatedAt,
	}
	if submission.State == model.TaskSubmissionStateConfirmed {
		view.PublicTaskID = submission.PublicTaskID
	}
	return view, nil
}

// GetTaskSubmissionUserView is the controller-facing owner-scoped recovery
// lookup. Foreign and malformed IDs both return nil so callers can respond 404
// without disclosing submission existence.
func GetTaskSubmissionUserView(userID int, clientRequestID string) (*TaskSubmissionUserView, error) {
	return GetTaskSubmissionForUser(userID, clientRequestID)
}

func normalizePrepareTaskSubmissionInput(input PrepareTaskSubmissionInput) (PrepareTaskSubmissionInput, error) {
	input, err := normalizeTaskSubmissionIdentity(input)
	if err != nil {
		return input, err
	}
	if input.DigestVersion == "" || len(input.DigestVersion) > 32 {
		return input, errors.New("invalid digest version")
	}
	if !validSHA256Hex(input.ProviderRequestDigest) || !validSHA256Hex(input.CredentialFingerprint) {
		return input, errors.New("invalid submission digest")
	}
	if input.PublicTaskID != "" && (!strings.HasPrefix(input.PublicTaskID, "task_") || len(input.PublicTaskID) > 191) {
		return input, errors.New("invalid public task id")
	}
	if input.ChannelID <= 0 || input.ChannelType <= 0 || input.Provider == "" || len(input.Provider) > 32 {
		return input, errors.New("invalid provider channel")
	}
	if input.LogicalAccountID == "" || len(input.LogicalAccountID) > 128 || input.CredentialRef == "" || len(input.CredentialRef) > 255 {
		return input, errors.New("invalid provider account reference")
	}
	baseOrigin, err := normalizeProviderBaseOrigin(input.CanonicalBaseOrigin)
	if err != nil {
		return input, err
	}
	input.CanonicalBaseOrigin = baseOrigin
	if input.OriginModelName == "" || len(input.OriginModelName) > 191 || input.UpstreamModelName == "" || len(input.UpstreamModelName) > 191 {
		return input, errors.New("invalid model snapshot")
	}
	if input.ReservedQuota <= 0 || input.ReservedQuota > common.MaxQuota {
		return input, errors.New("invalid reserved quota")
	}
	if input.FundingSource != BillingSourceWallet && input.FundingSource != BillingSourceSubscription {
		return input, errors.New("invalid funding source")
	}
	if input.FundingSource == BillingSourceSubscription && input.SubscriptionID < 0 {
		return input, errors.New("invalid subscription id")
	}
	if err := validateSubmissionRequestSummary(input.RequestSummary, input.Provider); err != nil {
		return input, err
	}
	if err := validateSubmissionPublicPriceSnapshot(input.PublicPriceSnapshot, input.OriginModelName, input.ReservedQuota); err != nil {
		return input, err
	}
	if err := validateSubmissionProviderCostSnapshot(input.ProviderCostSnapshot, input); err != nil {
		return input, err
	}
	return input, nil
}

func normalizeTaskSubmissionIdentity(input PrepareTaskSubmissionInput) (PrepareTaskSubmissionInput, error) {
	if input.UserID <= 0 {
		return input, errors.New("invalid user id")
	}
	parsedRequestID, err := uuid.Parse(strings.TrimSpace(input.ClientRequestID))
	if err != nil {
		return input, fmt.Errorf("invalid client request id: %w", err)
	}
	input.ClientRequestID = parsedRequestID.String()
	if !validSHA256Hex(input.RequestDigest) {
		return input, errors.New("invalid request digest")
	}
	return input, nil
}

func reserveSubmissionBalancesTx(tx *gorm.DB, submission *model.TaskSubmission) (*submissionBalanceSnapshot, error) {
	balances := &submissionBalanceSnapshot{}
	quota := submission.ReservedQuota
	now := common.GetTimestamp()

	switch submission.BillingSource {
	case BillingSourceWallet:
		var user model.User
		if err := submissionLockForUpdate(tx).Where("id = ?", submission.UserID).First(&user).Error; err != nil {
			return nil, err
		}
		if user.Status != common.UserStatusEnabled || user.Quota < quota {
			return nil, ErrTaskSubmissionInsufficientQuota
		}
		before := user.Quota
		after := before - quota
		if after < 0 {
			return nil, ErrTaskSubmissionInsufficientQuota
		}
		result := tx.Model(&model.User{}).Where("id = ? AND quota = ?", user.Id, before).Update("quota", after)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, ErrTaskSubmissionCASLost
		}
		balances.UserQuotaBefore = &before
		balances.UserQuotaAfter = &after
		balances.FundingReservedQuota = quota
	case BillingSourceSubscription:
		var subscription model.UserSubscription
		query := submissionLockForUpdate(tx).Where("user_id = ? AND status = ? AND end_time > ?", submission.UserID, "active", now)
		if submission.SubscriptionID != nil {
			query = query.Where("id = ?", *submission.SubscriptionID)
		} else {
			query = query.Order("end_time asc, id asc")
		}
		if err := query.First(&subscription).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrTaskSubmissionInsufficientQuota
			}
			return nil, err
		}
		before := subscription.AmountUsed
		after := before + int64(quota)
		if after < before || (subscription.AmountTotal > 0 && after > subscription.AmountTotal) {
			return nil, ErrTaskSubmissionInsufficientQuota
		}
		result := tx.Model(&model.UserSubscription{}).Where("id = ? AND amount_used = ?", subscription.Id, before).Update("amount_used", after)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, ErrTaskSubmissionCASLost
		}
		balances.SubscriptionUsedBefore = &before
		balances.SubscriptionUsedAfter = &after
		balances.FundingReservedQuota = quota
		submission.SubscriptionID = &subscription.Id
		if err := tx.Model(&model.TaskSubmission{}).Where("id = ?", submission.ID).Update("subscription_id", subscription.Id).Error; err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported funding source")
	}

	if submission.TokenID == nil {
		return balances, nil
	}
	var token model.Token
	if err := submissionLockForUpdate(tx).Where("id = ? AND user_id = ?", *submission.TokenID, submission.UserID).First(&token).Error; err != nil {
		return nil, err
	}
	if token.Status != common.TokenStatusEnabled {
		return nil, ErrTaskSubmissionInsufficientQuota
	}
	if token.UnlimitedQuota {
		return balances, nil
	}
	if token.RemainQuota < quota {
		return nil, ErrTaskSubmissionInsufficientQuota
	}
	remainBefore := token.RemainQuota
	remainAfter := remainBefore - quota
	usedBefore := token.UsedQuota
	usedAfter64 := int64(usedBefore) + int64(quota)
	if usedAfter64 > int64(common.MaxQuota) || usedAfter64 < 0 {
		return nil, ErrTaskSubmissionBalanceReconcile
	}
	usedAfter := int(usedAfter64)
	result := tx.Model(&model.Token{}).
		Where("id = ? AND user_id = ? AND remain_quota = ? AND used_quota = ?", token.Id, submission.UserID, remainBefore, usedBefore).
		Updates(map[string]any{"remain_quota": remainAfter, "used_quota": usedAfter, "accessed_time": now})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	balances.TokenQuotaBefore = &remainBefore
	balances.TokenQuotaAfter = &remainAfter
	balances.TokenUsedBefore = &usedBefore
	balances.TokenUsedAfter = &usedAfter
	balances.TokenReservedQuota = quota
	return balances, nil
}

func refundSubmissionTx(tx *gorm.DB, submissionID int64, expectedVersion int64, expectedState model.TaskSubmissionState, newState model.TaskSubmissionState, expectedBillingState model.TaskSubmissionBillingState, reasonCode string, actor string, providerNotApplicable bool) (*model.TaskSubmission, error) {
	if tx == nil {
		return nil, errors.New("nil transaction")
	}
	var submission model.TaskSubmission
	if err := submissionLockForUpdate(tx).Where("id = ?", submissionID).First(&submission).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskSubmissionNotFound
		}
		return nil, err
	}
	if submission.BillingState == model.TaskSubmissionBillingStateRefunded {
		return &submission, nil
	}
	if submission.Version != expectedVersion || submission.BillingState != expectedBillingState || (expectedState != "" && submission.State != expectedState) {
		return nil, ErrTaskSubmissionCASLost
	}

	balances, err := refundSubmissionBalancesTx(tx, &submission)
	if err != nil {
		return nil, err
	}
	versionAfter := expectedVersion + 1
	now := common.GetTimestamp()
	updates := map[string]any{
		"billing_state": model.TaskSubmissionBillingStateRefunded,
		"updated_at":    now,
		"version":       versionAfter,
	}
	if newState != "" {
		updates["state"] = newState
		updates["resolved_at"] = now
	}
	if expectedState == model.TaskSubmissionStateUnknown {
		updates["reconciled_at"] = now
		updates["reconciled_by"] = truncateSafeText(actor, 128)
		updates["reconcile_note"] = truncateSafeText(reasonCode, 512)
	}
	if providerNotApplicable {
		updates["provider_cost_state"] = model.TaskSubmissionProviderCostNotApplicable
	}
	if reasonCode != "" {
		updates["safe_error_code"] = truncateSafeText(reasonCode, 128)
	}
	query := tx.Model(&model.TaskSubmission{}).Where("id = ? AND version = ? AND billing_state = ?", submission.ID, expectedVersion, expectedBillingState)
	if expectedState != "" {
		query = query.Where("state = ?", expectedState)
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}

	entry := &model.TaskSubmissionBillingEntry{
		SubmissionID:            submission.ID,
		BillingRequestID:        submission.BillingRequestID,
		Operation:               model.TaskSubmissionBillingOperationRefund,
		Sequence:                1,
		UserID:                  submission.UserID,
		TokenID:                 submission.TokenID,
		SubscriptionID:          submission.SubscriptionID,
		FundingSource:           submission.BillingSource,
		FundingQuota:            submission.FundingReservedQuota,
		TokenQuota:              submission.TokenReservedQuota,
		BillingStateBefore:      expectedBillingState,
		BillingStateAfter:       model.TaskSubmissionBillingStateRefunded,
		UserQuotaBefore:         balances.UserQuotaBefore,
		UserQuotaAfter:          balances.UserQuotaAfter,
		TokenQuotaBefore:        balances.TokenQuotaBefore,
		TokenQuotaAfter:         balances.TokenQuotaAfter,
		TokenUsedBefore:         balances.TokenUsedBefore,
		TokenUsedAfter:          balances.TokenUsedAfter,
		SubscriptionUsedBefore:  balances.SubscriptionUsedBefore,
		SubscriptionUsedAfter:   balances.SubscriptionUsedAfter,
		SubmissionVersionBefore: expectedVersion,
		SubmissionVersionAfter:  versionAfter,
		ReasonCode:              truncateSafeText(reasonCode, 128),
		Actor:                   truncateSafeText(actor, 128),
	}
	if entry.Actor == "" {
		entry.Actor = "system"
	}
	if err := tx.Create(entry).Error; err != nil {
		return nil, err
	}
	if err := enqueueSubmissionQuotaInvalidationTx(tx, entry); err != nil {
		return nil, err
	}
	if err := tx.First(&submission, submission.ID).Error; err != nil {
		return nil, err
	}
	return &submission, nil
}

func refundSubmissionBalancesTx(tx *gorm.DB, submission *model.TaskSubmission) (*submissionBalanceSnapshot, error) {
	balances := &submissionBalanceSnapshot{
		FundingReservedQuota: submission.FundingReservedQuota,
		TokenReservedQuota:   submission.TokenReservedQuota,
	}
	if submission.FundingReservedQuota < 0 || submission.TokenReservedQuota < 0 {
		return nil, ErrTaskSubmissionBalanceReconcile
	}
	switch submission.BillingSource {
	case BillingSourceWallet:
		var user model.User
		if err := submissionLockForUpdate(tx.Unscoped()).Where("id = ?", submission.UserID).First(&user).Error; err != nil {
			return nil, err
		}
		before := user.Quota
		after64 := int64(before) + int64(submission.FundingReservedQuota)
		if after64 > int64(common.MaxQuota) || after64 < 0 {
			return nil, ErrTaskSubmissionBalanceReconcile
		}
		after := int(after64)
		result := tx.Unscoped().Model(&model.User{}).Where("id = ? AND quota = ?", user.Id, before).Update("quota", after)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, ErrTaskSubmissionCASLost
		}
		balances.UserQuotaBefore = &before
		balances.UserQuotaAfter = &after
	case BillingSourceSubscription:
		if submission.SubscriptionID == nil {
			return nil, ErrTaskSubmissionBalanceReconcile
		}
		var subscription model.UserSubscription
		if err := submissionLockForUpdate(tx).Where("id = ? AND user_id = ?", *submission.SubscriptionID, submission.UserID).First(&subscription).Error; err != nil {
			return nil, err
		}
		before := subscription.AmountUsed
		if before < int64(submission.FundingReservedQuota) {
			return nil, ErrTaskSubmissionBalanceReconcile
		}
		after := before - int64(submission.FundingReservedQuota)
		result := tx.Model(&model.UserSubscription{}).Where("id = ? AND amount_used = ?", subscription.Id, before).Update("amount_used", after)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, ErrTaskSubmissionCASLost
		}
		balances.SubscriptionUsedBefore = &before
		balances.SubscriptionUsedAfter = &after
	default:
		return nil, ErrTaskSubmissionBalanceReconcile
	}

	if submission.TokenReservedQuota == 0 {
		return balances, nil
	}
	if submission.TokenID == nil {
		return nil, ErrTaskSubmissionBalanceReconcile
	}
	var token model.Token
	if err := submissionLockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", *submission.TokenID, submission.UserID).First(&token).Error; err != nil {
		return nil, err
	}
	remainBefore := token.RemainQuota
	remainAfter64 := int64(remainBefore) + int64(submission.TokenReservedQuota)
	if remainAfter64 > int64(common.MaxQuota) || remainAfter64 < 0 || token.UsedQuota < submission.TokenReservedQuota {
		return nil, ErrTaskSubmissionBalanceReconcile
	}
	remainAfter := int(remainAfter64)
	usedBefore := token.UsedQuota
	usedAfter := usedBefore - submission.TokenReservedQuota
	tokenUpdates := map[string]any{
		"remain_quota":  remainAfter,
		"used_quota":    usedAfter,
		"accessed_time": common.GetTimestamp(),
	}
	// ValidateUserToken may persist EXHAUSTED after an exact-balance T1. A
	// verified refund that makes quota positive must restore only that derived
	// state; never override DISABLED or EXPIRED, which are independent policy
	// decisions.
	if token.Status == common.TokenStatusExhausted && remainAfter > 0 {
		tokenUpdates["status"] = common.TokenStatusEnabled
	}
	result := tx.Unscoped().Model(&model.Token{}).
		Where("id = ? AND user_id = ? AND remain_quota = ? AND used_quota = ?", token.Id, submission.UserID, remainBefore, usedBefore).
		Updates(tokenUpdates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrTaskSubmissionCASLost
	}
	balances.TokenQuotaBefore = &remainBefore
	balances.TokenQuotaAfter = &remainAfter
	balances.TokenUsedBefore = &usedBefore
	balances.TokenUsedAfter = &usedAfter
	return balances, nil
}

func billingTransitionEntry(submission *model.TaskSubmission, operation model.TaskSubmissionBillingOperation, before model.TaskSubmissionBillingState, after model.TaskSubmissionBillingState, versionBefore int64, versionAfter int64, reasonCode string, actor string) *model.TaskSubmissionBillingEntry {
	return &model.TaskSubmissionBillingEntry{
		SubmissionID:            submission.ID,
		BillingRequestID:        submission.BillingRequestID,
		Operation:               operation,
		Sequence:                1,
		UserID:                  submission.UserID,
		TokenID:                 submission.TokenID,
		SubscriptionID:          submission.SubscriptionID,
		FundingSource:           submission.BillingSource,
		FundingQuota:            submission.FundingReservedQuota,
		TokenQuota:              submission.TokenReservedQuota,
		BillingStateBefore:      before,
		BillingStateAfter:       after,
		SubmissionVersionBefore: versionBefore,
		SubmissionVersionAfter:  versionAfter,
		ReasonCode:              truncateSafeText(reasonCode, 128),
		Actor:                   truncateSafeText(actor, 128),
	}
}

func enqueueSubmissionQuotaInvalidationTx(tx *gorm.DB, entry *model.TaskSubmissionBillingEntry) error {
	if entry == nil || entry.ID <= 0 || entry.UserID <= 0 {
		return errors.New("invalid billing entry for quota invalidation")
	}
	event := &model.QuotaCacheInvalidationOutbox{
		EventKey:       fmt.Sprintf("task_submission_billing:%d:user:%d", entry.ID, entry.UserID),
		AggregateType:  model.QuotaCacheAggregateUserBilling,
		AggregateID:    int64(entry.UserID),
		BalanceVersion: entry.ID,
	}
	return tx.Create(event).Error
}

func submissionLockForUpdate(tx *gorm.DB) *gorm.DB {
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return tx
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"})
}

func marshalSubmissionSnapshot(value any) (string, error) {
	data, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func normalizeProviderBaseOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid provider base origin")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("provider base origin must not include a path")
	}
	return "https://" + strings.ToLower(parsed.Host), nil
}

func validateSubmissionRequestSummary(summary SubmissionRequestSummary, provider string) error {
	if summary.SchemaVersion <= 0 || summary.Action == "" || len(summary.Action) > 40 {
		return errors.New("invalid request summary")
	}
	if summary.Duration < 1 || summary.Duration > 15 || summary.ImageCount < 0 || summary.ImageCount > 9 || summary.VideoCount < 0 || summary.VideoCount > 3 || summary.AudioCount < 0 || summary.AudioCount > 1 {
		return errors.New("request summary exceeds public bounds")
	}
	if provider == "wxmaas-seedance" && summary.Duration < 4 {
		return errors.New("wxmaas duration must be between 4 and 15 seconds")
	}
	if strings.ToLower(summary.Resolution) != "720p" {
		return errors.New("only 720p is enabled for the first release")
	}
	allowedRatios := map[string]struct{}{"16:9": {}, "9:16": {}, "1:1": {}, "4:3": {}, "3:4": {}}
	if _, ok := allowedRatios[summary.Ratio]; !ok {
		return errors.New("unsupported aspect ratio")
	}
	return nil
}

func validateSubmissionPublicPriceSnapshot(snapshot SubmissionPublicPriceSnapshot, originModel string, reservedQuota int) error {
	if snapshot.SchemaVersion <= 0 || snapshot.PriceVersion == "" || len(snapshot.PriceVersion) > 128 || snapshot.ModelName != originModel || snapshot.ProductQuota != reservedQuota || snapshot.ProductQuota <= 0 {
		return errors.New("invalid public price snapshot")
	}
	ratio, err := decimal.NewFromString(snapshot.GroupRatio)
	if err != nil || !ratio.IsPositive() {
		return errors.New("invalid group ratio snapshot")
	}
	if snapshot.Currency == "" || len(snapshot.Currency) > 8 {
		return errors.New("invalid public price currency")
	}
	return nil
}

func validateSubmissionProviderCostSnapshot(snapshot SubmissionProviderCostSnapshot, input PrepareTaskSubmissionInput) error {
	if snapshot.SchemaVersion <= 0 || snapshot.ProviderPriceKey == "" || snapshot.ProviderPriceVersion == "" || snapshot.Provider != input.Provider || snapshot.UpstreamModelName != input.UpstreamModelName {
		return errors.New("invalid provider cost snapshot")
	}
	if snapshot.Resolution != input.RequestSummary.Resolution || snapshot.Duration != input.RequestSummary.Duration || snapshot.ImageCount != input.RequestSummary.ImageCount || snapshot.VideoCount != input.RequestSummary.VideoCount || snapshot.AudioCount != input.RequestSummary.AudioCount || snapshot.HasVideo != (input.RequestSummary.VideoCount > 0) {
		return errors.New("provider cost snapshot does not match request summary")
	}
	unitPrice, err := decimal.NewFromString(snapshot.UnitPricePerMillionCNY)
	if err != nil || unitPrice.IsNegative() {
		return errors.New("invalid provider unit price")
	}
	if snapshot.RoundingRule == "" || len(snapshot.RoundingRule) > 64 {
		return errors.New("invalid provider rounding rule")
	}
	manualReconcile := snapshot.RoundingRule == "MANUAL_RECONCILE"
	if manualReconcile {
		if input.Provider == "wxmaas-seedance" || !unitPrice.IsZero() {
			return errors.New("manual provider cost reconciliation is not valid for this channel")
		}
		return nil
	}
	if !unitPrice.IsPositive() {
		return errors.New("provider unit price must be positive")
	}
	return nil
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validProviderTaskID(value string) bool {
	if value == "" || len(value) > 191 || strings.ContainsAny(value, "/\\?#\x00\r\n\t ") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func validArchivedResultRef(value string) bool {
	return ValidateSD2ArchivedResultRef(value) == nil
}

func nullablePositiveInt(value int) *int {
	if value <= 0 {
		return nil
	}
	copy := value
	return &copy
}

func truncateSafeText(value string, max int) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\t", " ")
	runes := []rune(value)
	if len(runes) > max {
		value = string(runes[:max])
	}
	return value
}

func sanitizeSubmissionErrorMessage(value string) string {
	value = truncateSafeText(value, 512)
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization", "bearer ", "api-key", "api_key", "sk-", "http://", "https://", "x-amz-"} {
		if strings.Contains(lower, marker) {
			return "the provider submission outcome requires reconciliation"
		}
	}
	return value
}
