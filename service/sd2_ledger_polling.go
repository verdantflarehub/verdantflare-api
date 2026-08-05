package service

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"gorm.io/gorm"
)

const (
	SD2LedgerPollMutationProgress           = "PROGRESS"
	SD2LedgerPollMutationTransientIssue     = "TRANSIENT_ISSUE"
	SD2LedgerPollMutationArchiveFailed      = "ARCHIVE_FAILED"
	SD2LedgerPollMutationArchivedCompleted  = "ARCHIVED_COMPLETED"
	SD2LedgerPollMutationProviderFailed     = "PROVIDER_FAILED"
	SD2LedgerPollMutationProviderUnresolved = "PROVIDER_UNRESOLVED"
)

// SD2LedgerTaskLink is the immutable routing and optimistic-lock context needed
// by the poller. AllowedResultHosts must come from operator-controlled channel
// configuration; an empty list deliberately makes result archival fail closed.
type SD2LedgerTaskLink struct {
	SubmissionID          int64
	Version               int64
	TaskDBID              int64
	PublicTaskID          string
	UpstreamTaskID        string
	UserID                int
	ChannelID             int
	ChannelType           int
	CanonicalBaseOrigin   string
	CredentialFingerprint string
	PollState             string
	PollAttempts          int
	NextPollAt            int64
	Provider              string
	AllowedResultHosts    []string
}

// SD2LedgerPollMutation is applied atomically by the ledger store. Only
// ARCHIVED_COMPLETED may publish Task SUCCESS and settle user billing.
// PROVIDER_FAILED applies the approved D9 no-deliverable refund policy;
// PROVIDER_UNRESOLVED never refunds.
type SD2LedgerPollMutation struct {
	Kind           string
	ExpectedStatus model.TaskStatus
	TaskStatus     model.TaskStatus
	Progress       string
	StartTime      int64
	FinishTime     int64
	PollState      string
	NextPollAt     int64
	SafeErrorCode  string
	Snapshot       *SD2TaskResultSnapshot
	ArchivedResult *SD2ArchivedResult
}

type SD2LedgerPollingStore interface {
	LookupByTaskDBID(ctx context.Context, taskDBID int64) (SD2LedgerTaskLink, bool, error)
	ApplyPollMutation(ctx context.Context, link SD2LedgerTaskLink, mutation SD2LedgerPollMutation) (bool, error)
}

type SD2ResultHostAllowlistResolver func(submission *model.TaskSubmission) []string

var sd2LedgerPollingStoreRegistry struct {
	sync.RWMutex
	store SD2LedgerPollingStore
}

var sd2ResultHostAllowlistRegistry struct {
	sync.RWMutex
	resolver SD2ResultHostAllowlistResolver
}

func init() {
	SetSD2LedgerPollingStore(modelSD2LedgerPollingStore{})
}

func SetSD2LedgerPollingStore(store SD2LedgerPollingStore) {
	sd2LedgerPollingStoreRegistry.Lock()
	defer sd2LedgerPollingStoreRegistry.Unlock()
	sd2LedgerPollingStoreRegistry.store = store
}

// SetSD2ResultHostAllowlistResolver wires operator-controlled result hosts.
// The default is nil/empty, which intentionally prevents provider success from
// becoming completed until the private archive deployment is configured.
func SetSD2ResultHostAllowlistResolver(resolver SD2ResultHostAllowlistResolver) {
	sd2ResultHostAllowlistRegistry.Lock()
	defer sd2ResultHostAllowlistRegistry.Unlock()
	sd2ResultHostAllowlistRegistry.resolver = resolver
}

func SD2ResultHostsForSubmission(submission *model.TaskSubmission) []string {
	if submission == nil {
		return nil
	}
	sd2ResultHostAllowlistRegistry.RLock()
	resolver := sd2ResultHostAllowlistRegistry.resolver
	sd2ResultHostAllowlistRegistry.RUnlock()
	if resolver == nil {
		return nil
	}

	seen := make(map[string]struct{})
	allowedHosts := make([]string, 0)
	for _, configuredHost := range resolver(submission) {
		configuredHost = strings.ToLower(strings.TrimSpace(configuredHost))
		parsed, err := url.Parse("https://" + configuredHost)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" {
			continue
		}
		host := parsed.Hostname()
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		allowedHosts = append(allowedHosts, host)
	}
	return allowedHosts
}

func ValidateSD2ResultDeliveryReady(submission *model.TaskSubmission) error {
	if !IsSD2ResultArchiveConfigured() {
		return ErrSD2ResultArchiveUnavailable
	}
	if len(SD2ResultHostsForSubmission(submission)) == 0 {
		return fmt.Errorf("sd2 provider result host allowlist is not configured")
	}
	return nil
}

// SD2ChannelCredentialFingerprint returns the non-reversible value frozen in a
// submission. It lets polling detect accidental account/key switches without
// persisting the channel key in Task or TaskSubmission.
func SD2ChannelCredentialFingerprint(key string) string {
	identityKey, err := sd2RequestIdentityHMACKey()
	if err != nil {
		return ""
	}
	return common.GenerateHMACWithKey(identityKey, "sd2-channel-credential-v1\x00"+strings.TrimSpace(key))
}

func validateSD2LedgerChannelBinding(link SD2LedgerTaskLink, channel *model.Channel) string {
	if channel == nil || channel.Id != link.ChannelID || channel.Type != link.ChannelType {
		return "channel_binding_changed"
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.ChannelBaseURLs[channel.Type]
	}
	baseOrigin, err := normalizeProviderBaseOrigin(baseURL)
	if err != nil || baseOrigin != link.CanonicalBaseOrigin {
		return "channel_base_origin_changed"
	}
	currentFingerprint := SD2ChannelCredentialFingerprint(channel.Key)
	if link.CredentialFingerprint == "" || !hmac.Equal([]byte(currentFingerprint), []byte(link.CredentialFingerprint)) {
		return "channel_credential_changed"
	}
	return ""
}

func lookupSD2LedgerTask(ctx context.Context, taskDBID int64) (SD2LedgerTaskLink, bool, error) {
	sd2LedgerPollingStoreRegistry.RLock()
	store := sd2LedgerPollingStoreRegistry.store
	sd2LedgerPollingStoreRegistry.RUnlock()
	if store == nil {
		return SD2LedgerTaskLink{}, false, nil
	}
	return store.LookupByTaskDBID(ctx, taskDBID)
}

func applySD2LedgerPollMutation(ctx context.Context, link SD2LedgerTaskLink, mutation SD2LedgerPollMutation) (bool, error) {
	sd2LedgerPollingStoreRegistry.RLock()
	store := sd2LedgerPollingStoreRegistry.store
	sd2LedgerPollingStoreRegistry.RUnlock()
	if store == nil {
		return false, fmt.Errorf("sd2 ledger polling store is not configured")
	}
	if mutation.Kind != SD2LedgerPollMutationArchivedCompleted {
		mutation.ArchivedResult = nil
	}
	return store.ApplyPollMutation(ctx, link, mutation)
}

func newSD2SnapshotFromTaskResult(taskResult *relaycommon.TaskInfo) SD2TaskResultSnapshot {
	snapshot := NewSD2TaskResultSnapshot()
	if taskResult == nil {
		return snapshot
	}
	snapshot.ProviderStatus = strings.ToLower(strings.TrimSpace(taskResult.Status))
	switch model.TaskStatus(taskResult.Status) {
	case model.TaskStatusSuccess:
		snapshot.ProviderStatus = "succeeded"
	case model.TaskStatusFailure:
		snapshot.ProviderStatus = "failed"
	case model.TaskStatusInProgress:
		snapshot.ProviderStatus = "running"
	case model.TaskStatusQueued, model.TaskStatusSubmitted:
		snapshot.ProviderStatus = "queued"
	}
	switch strings.TrimSpace(taskResult.Reason) {
	case "provider_expired":
		snapshot.ProviderStatus = "expired"
		snapshot.SafeErrorCode = "provider_expired"
	case "provider_cancelled":
		snapshot.ProviderStatus = "cancelled"
		snapshot.SafeErrorCode = "provider_cancelled"
	}
	snapshot.Usage = SD2TaskUsageSnapshot{
		CompletionTokens: int64(taskResult.CompletionTokens),
		TotalTokens:      int64(taskResult.TotalTokens),
	}
	snapshot.Seed = taskResult.Seed
	snapshot.Resolution = strings.TrimSpace(taskResult.Resolution)
	snapshot.Duration = taskResult.Duration
	snapshot.Ratio = strings.TrimSpace(taskResult.Ratio)
	snapshot.FramesPerSecond = taskResult.FramesPerSecond
	snapshot.GenerateAudio = taskResult.GenerateAudio
	snapshot.ProviderCreatedAt = taskResult.ProviderCreatedAt
	snapshot.ProviderUpdatedAt = taskResult.ProviderUpdatedAt
	return snapshot
}

func sd2PollBackoff(link SD2LedgerTaskLink, now time.Time) int64 {
	attempts := link.PollAttempts
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 {
		attempts = 6
	}
	delay := 15 * time.Second * time.Duration(1<<attempts)
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	return now.Add(delay).Unix()
}

func markSD2LedgerPollIssue(ctx context.Context, task *model.Task, link SD2LedgerTaskLink, pollState string, safeCode string) error {
	won, err := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
		Kind:           SD2LedgerPollMutationTransientIssue,
		ExpectedStatus: task.Status,
		TaskStatus:     task.Status,
		Progress:       task.Progress,
		PollState:      pollState,
		NextPollAt:     sd2PollBackoff(link, time.Now()),
		SafeErrorCode:  safeCode,
	})
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("sd2 ledger poll mutation lost optimistic lock")
	}
	return nil
}

func handleSD2LedgerTaskResult(ctx context.Context, task *model.Task, link SD2LedgerTaskLink, taskResult *relaycommon.TaskInfo) error {
	if taskResult == nil {
		return markSD2LedgerPollIssue(ctx, task, link, "BACKOFF", "invalid_provider_result")
	}

	now := time.Now().Unix()
	snapshot := newSD2SnapshotFromTaskResult(taskResult)
	if snapshot.SafeErrorCode == "provider_expired" || snapshot.SafeErrorCode == "provider_cancelled" {
		return applySD2ProviderUnresolved(ctx, task, link, snapshot)
	}
	switch model.TaskStatus(taskResult.Status) {
	case model.TaskStatusSubmitted:
		return applySD2Progress(ctx, task, link, taskcommon.ProgressSubmitted, snapshot, 0)
	case model.TaskStatusQueued:
		return applySD2Progress(ctx, task, link, taskcommon.ProgressQueued, snapshot, 0)
	case model.TaskStatusInProgress:
		startTime := task.StartTime
		if startTime == 0 {
			startTime = now
		}
		return applySD2Progress(ctx, task, link, taskcommon.ProgressInProgress, snapshot, startTime)
	case model.TaskStatusSuccess:
		if strings.TrimSpace(taskResult.Url) == "" {
			return markSD2LedgerPollIssue(ctx, task, link, "BACKOFF", "provider_result_missing")
		}
		snapshot.ArchiveState = SD2ArchiveStatePending
		archived, err := ArchiveSD2ProviderResult(ctx, SD2ArchiveRequest{
			PublicTaskID:       task.TaskID,
			UserID:             task.UserId,
			SourceURL:          taskResult.Url,
			AllowedSourceHosts: append([]string(nil), link.AllowedResultHosts...),
		})
		if err != nil {
			snapshot.ArchiveState = SD2ArchiveStateFailed
			snapshot.SafeErrorCode = "result_archive_failed"
			won, mutationErr := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
				Kind:           SD2LedgerPollMutationArchiveFailed,
				ExpectedStatus: task.Status,
				TaskStatus:     task.Status,
				Progress:       task.Progress,
				PollState:      "BACKOFF",
				NextPollAt:     sd2PollBackoff(link, time.Now()),
				SafeErrorCode:  snapshot.SafeErrorCode,
				Snapshot:       &snapshot,
			})
			if mutationErr != nil {
				return fmt.Errorf("record sd2 result archive failure: %w", mutationErr)
			}
			if !won {
				return fmt.Errorf("record sd2 result archive failure lost optimistic lock")
			}
			return fmt.Errorf("sd2 result archival failed")
		}

		snapshot.ArchiveState = SD2ArchiveStateArchived
		snapshot.ArchivedResult = &archived
		won, err := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
			Kind:           SD2LedgerPollMutationArchivedCompleted,
			ExpectedStatus: task.Status,
			TaskStatus:     model.TaskStatusSuccess,
			Progress:       taskcommon.ProgressComplete,
			FinishTime:     now,
			PollState:      "TERMINAL",
			Snapshot:       &snapshot,
			ArchivedResult: &archived,
		})
		if err != nil {
			return err
		}
		if !won {
			return fmt.Errorf("complete sd2 archived result lost optimistic lock")
		}
		return nil
	case model.TaskStatusFailure:
		snapshot.SafeErrorCode = "provider_failed"
		snapshot.ArchiveState = SD2ArchiveStateFailed
		won, err := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
			Kind:           SD2LedgerPollMutationProviderFailed,
			ExpectedStatus: task.Status,
			TaskStatus:     model.TaskStatusFailure,
			Progress:       taskcommon.ProgressComplete,
			FinishTime:     now,
			PollState:      "TERMINAL",
			SafeErrorCode:  snapshot.SafeErrorCode,
			Snapshot:       &snapshot,
		})
		if err != nil {
			return err
		}
		if !won {
			return fmt.Errorf("fail sd2 provider task lost optimistic lock")
		}
		return nil
	default:
		if snapshot.SafeErrorCode == "" {
			snapshot.SafeErrorCode = "provider_status_unrecognized"
		}
		return applySD2ProviderUnresolved(ctx, task, link, snapshot)
	}
}

func applySD2ProviderUnresolved(ctx context.Context, task *model.Task, link SD2LedgerTaskLink, snapshot SD2TaskResultSnapshot) error {
	taskStatus := task.Status
	progress := task.Progress
	pollState := string(model.TaskSubmissionPollStateBackoff)
	nextPollAt := sd2PollBackoff(link, time.Now())
	finishTime := int64(0)
	if snapshot.SafeErrorCode == "provider_expired" || snapshot.SafeErrorCode == "provider_cancelled" {
		// These are provider-declared terminal states, but they do not prove that
		// no provider cost was incurred. Stop exposing an eternally-running task,
		// retain the committed user charge, and require provider-cost evidence.
		taskStatus = model.TaskStatusFailure
		progress = taskcommon.ProgressComplete
		pollState = string(model.TaskSubmissionPollStateTerminal)
		nextPollAt = 0
		finishTime = time.Now().Unix()
	}
	won, err := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
		Kind:           SD2LedgerPollMutationProviderUnresolved,
		ExpectedStatus: task.Status,
		TaskStatus:     taskStatus,
		Progress:       progress,
		FinishTime:     finishTime,
		PollState:      pollState,
		NextPollAt:     nextPollAt,
		SafeErrorCode:  snapshot.SafeErrorCode,
		Snapshot:       &snapshot,
	})
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("record unresolved sd2 provider state lost optimistic lock")
	}
	return nil
}

func applySD2Progress(ctx context.Context, task *model.Task, link SD2LedgerTaskLink, progress string, snapshot SD2TaskResultSnapshot, startTime int64) error {
	won, err := applySD2LedgerPollMutation(ctx, link, SD2LedgerPollMutation{
		Kind:           SD2LedgerPollMutationProgress,
		ExpectedStatus: task.Status,
		TaskStatus:     model.TaskStatus(snapshotToTaskStatus(snapshot.ProviderStatus, task.Status)),
		Progress:       progress,
		StartTime:      startTime,
		PollState:      "ACTIVE",
		Snapshot:       &snapshot,
	})
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("update sd2 task progress lost optimistic lock")
	}
	return nil
}

func snapshotToTaskStatus(providerStatus string, fallback model.TaskStatus) model.TaskStatus {
	providerStatus = strings.ToLower(strings.TrimSpace(providerStatus))
	if providerStatus == "running" {
		return model.TaskStatusInProgress
	}
	status := model.TaskStatus(strings.ToUpper(providerStatus))
	switch status {
	case model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress:
		return status
	default:
		return fallback
	}
}

type modelSD2LedgerPollingStore struct{}

func (modelSD2LedgerPollingStore) LookupByTaskDBID(_ context.Context, taskDBID int64) (SD2LedgerTaskLink, bool, error) {
	submission, err := model.GetTaskSubmissionByTaskDBID(taskDBID)
	if err != nil {
		return SD2LedgerTaskLink{}, false, err
	}
	if submission == nil {
		return SD2LedgerTaskLink{}, false, nil
	}

	allowedHosts := SD2ResultHostsForSubmission(submission)
	nextPollAt := int64(0)
	if submission.NextPollAt != nil {
		nextPollAt = *submission.NextPollAt
	}
	upstreamTaskID := ""
	if submission.UpstreamTaskID != nil {
		upstreamTaskID = *submission.UpstreamTaskID
	}

	return SD2LedgerTaskLink{
		SubmissionID:          submission.ID,
		Version:               submission.Version,
		TaskDBID:              taskDBID,
		PublicTaskID:          submission.PublicTaskID,
		UpstreamTaskID:        upstreamTaskID,
		UserID:                submission.UserID,
		ChannelID:             submission.ChannelID,
		ChannelType:           submission.ChannelType,
		CanonicalBaseOrigin:   submission.CanonicalBaseOrigin,
		CredentialFingerprint: submission.CredentialFingerprint,
		PollState:             string(submission.PollState),
		PollAttempts:          submission.PollAttempts,
		NextPollAt:            nextPollAt,
		Provider:              submission.Provider,
		AllowedResultHosts:    allowedHosts,
	}, true, nil
}

func (modelSD2LedgerPollingStore) ApplyPollMutation(_ context.Context, link SD2LedgerTaskLink, mutation SD2LedgerPollMutation) (bool, error) {
	if link.SubmissionID <= 0 || link.TaskDBID <= 0 || link.Version <= 0 {
		return false, fmt.Errorf("invalid sd2 ledger link")
	}
	if mutation.Snapshot != nil {
		if err := mutation.Snapshot.Validate(); err != nil {
			return false, err
		}
	}

	won := false
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var submission model.TaskSubmission
		if err := submissionLockForUpdate(tx).Where("id = ?", link.SubmissionID).First(&submission).Error; err != nil {
			return err
		}
		if submission.Version != link.Version || submission.State != model.TaskSubmissionStateConfirmed || submission.TaskDBID == nil || *submission.TaskDBID != link.TaskDBID || submission.PublicTaskID != link.PublicTaskID || submission.UserID != link.UserID || submission.ChannelID != link.ChannelID {
			return ErrTaskSubmissionCASLost
		}

		var task model.Task
		if err := submissionLockForUpdate(tx).Where("id = ?", link.TaskDBID).First(&task).Error; err != nil {
			return err
		}
		if task.TaskID != link.PublicTaskID || task.UserId != link.UserID || task.ChannelId != link.ChannelID || task.Status != mutation.ExpectedStatus {
			return ErrTaskSubmissionCASLost
		}

		now := common.GetTimestamp()
		nextPollAt := any(nil)
		if mutation.NextPollAt > 0 {
			nextPollAt = mutation.NextPollAt
		}
		baseSubmissionUpdates := map[string]any{
			"poll_state":      model.TaskSubmissionPollState(mutation.PollState),
			"next_poll_at":    nextPollAt,
			"last_poll_at":    now,
			"poll_attempts":   gorm.Expr("poll_attempts + ?", 1),
			"safe_error_code": mutation.SafeErrorCode,
			"updated_at":      now,
		}
		if mutation.Snapshot != nil {
			baseSubmissionUpdates["provider_status"] = mutation.Snapshot.ProviderStatus
			baseSubmissionUpdates["archive_state"] = model.TaskSubmissionArchiveState(mutation.Snapshot.ArchiveState)
			baseSubmissionUpdates["result_snapshot_version"] = mutation.Snapshot.SchemaVersion
		}

		switch mutation.Kind {
		case SD2LedgerPollMutationTransientIssue:
			if mutation.PollState == string(model.TaskSubmissionPollStateStale) {
				baseSubmissionUpdates["provider_cost_state"] = model.TaskSubmissionProviderCostReconcileRequired
			}
			return updateSD2SubmissionPollTx(tx, &submission, link.Version, baseSubmissionUpdates)

		case SD2LedgerPollMutationProgress, SD2LedgerPollMutationArchiveFailed, SD2LedgerPollMutationProviderUnresolved:
			if mutation.Snapshot == nil {
				return fmt.Errorf("sd2 poll mutation requires a result snapshot")
			}
			if mutation.Kind == SD2LedgerPollMutationProviderUnresolved {
				baseSubmissionUpdates["provider_cost_state"] = model.TaskSubmissionProviderCostReconcileRequired
			}
			if err := updateSD2TaskObservationTx(tx, &task, mutation, now); err != nil {
				return err
			}
			if mutation.Kind == SD2LedgerPollMutationArchiveFailed {
				providerCostUpdates, err := archivedSubmissionProviderCostUpdatesTx(tx, &submission)
				if err != nil {
					return err
				}
				for field, value := range providerCostUpdates {
					baseSubmissionUpdates[field] = value
				}
			}
			return updateSD2SubmissionPollTx(tx, &submission, link.Version, baseSubmissionUpdates)

		case SD2LedgerPollMutationArchivedCompleted:
			if mutation.Snapshot == nil || mutation.ArchivedResult == nil ||
				mutation.Snapshot.ArchiveState != SD2ArchiveStateArchived ||
				mutation.Snapshot.ArchivedResult == nil ||
				*mutation.Snapshot.ArchivedResult != *mutation.ArchivedResult {
				return fmt.Errorf("sd2 completed mutation requires a verified archived result")
			}
			if mutation.TaskStatus != model.TaskStatusSuccess || mutation.PollState != string(model.TaskSubmissionPollStateTerminal) {
				return fmt.Errorf("invalid sd2 completed mutation state")
			}
			if err := updateSD2TaskObservationTx(tx, &task, mutation, now); err != nil {
				return err
			}
			if _, err := ArchiveAndSettleSubmissionTx(tx, submission.ID, link.Version, *mutation.ArchivedResult, "sd2-poller"); err != nil {
				return err
			}
			finalUpdates := map[string]any{
				"provider_status":         mutation.Snapshot.ProviderStatus,
				"safe_error_code":         "",
				"result_snapshot_version": mutation.Snapshot.SchemaVersion,
				"last_poll_at":            now,
				"next_poll_at":            nil,
				"updated_at":              now,
			}
			if mutation.Snapshot.Usage.TotalTokens <= 0 {
				finalUpdates["provider_cost_state"] = model.TaskSubmissionProviderCostReconcileRequired
			}
			if err := tx.Model(&model.TaskSubmission{}).Where("id = ?", submission.ID).Updates(finalUpdates).Error; err != nil {
				return err
			}
			won = true
			return nil

		case SD2LedgerPollMutationProviderFailed:
			if mutation.Snapshot == nil || mutation.TaskStatus != model.TaskStatusFailure {
				return fmt.Errorf("invalid sd2 provider failure mutation")
			}
			if err := updateSD2TaskObservationTx(tx, &task, mutation, now); err != nil {
				return err
			}
			providerCostUpdates, err := archivedSubmissionProviderCostUpdatesTx(tx, &submission)
			if err != nil {
				return err
			}
			refunded, err := RefundSubmissionTx(tx, submission.ID, link.Version, model.TaskSubmissionBillingStateCommitted, "provider_failed_no_deliverable", "sd2-poller")
			if err != nil {
				return err
			}
			failureUpdates := map[string]any{
				"poll_state":              model.TaskSubmissionPollStateTerminal,
				"provider_status":         mutation.Snapshot.ProviderStatus,
				"archive_state":           model.TaskSubmissionArchiveStateFailed,
				"result_snapshot_version": mutation.Snapshot.SchemaVersion,
				"safe_error_code":         mutation.SafeErrorCode,
				"last_poll_at":            now,
				"next_poll_at":            nil,
				"updated_at":              now,
			}
			for field, value := range providerCostUpdates {
				failureUpdates[field] = value
			}
			if err := tx.Model(&model.TaskSubmission{}).Where("id = ?", submission.ID).Updates(failureUpdates).Error; err != nil {
				return err
			}
			if refunded == nil {
				return ErrTaskSubmissionCASLost
			}
			won = true
			return nil

		default:
			return fmt.Errorf("unknown sd2 ledger poll mutation %q", mutation.Kind)
		}
	})
	if err != nil {
		if errors.Is(err, ErrTaskSubmissionCASLost) {
			return false, nil
		}
		return false, err
	}
	if mutation.Kind != SD2LedgerPollMutationArchivedCompleted && mutation.Kind != SD2LedgerPollMutationProviderFailed {
		won = true
	}
	return won, nil
}

func updateSD2SubmissionPollTx(tx *gorm.DB, submission *model.TaskSubmission, expectedVersion int64, updates map[string]any) error {
	updates["version"] = gorm.Expr("version + ?", 1)
	result := tx.Model(&model.TaskSubmission{}).
		Where("id = ? AND version = ? AND state = ?", submission.ID, expectedVersion, model.TaskSubmissionStateConfirmed).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskSubmissionCASLost
	}
	return nil
}

func updateSD2TaskObservationTx(tx *gorm.DB, task *model.Task, mutation SD2LedgerPollMutation, now int64) error {
	if mutation.Snapshot == nil {
		return fmt.Errorf("missing sd2 result snapshot")
	}
	snapshotData, err := EncodeSD2TaskResultSnapshot(*mutation.Snapshot)
	if err != nil {
		return err
	}
	task.Data = snapshotData
	updates := map[string]any{
		"status":     mutation.TaskStatus,
		"progress":   mutation.Progress,
		"data":       task.Data,
		"updated_at": now,
	}
	if mutation.StartTime > 0 {
		updates["start_time"] = mutation.StartTime
	}
	if mutation.FinishTime > 0 {
		updates["finish_time"] = mutation.FinishTime
	}
	if mutation.Kind == SD2LedgerPollMutationArchivedCompleted {
		privateData := task.PrivateData
		privateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		updates["private_data"] = privateData
		updates["fail_reason"] = ""
	} else if mutation.Kind == SD2LedgerPollMutationProviderFailed {
		updates["fail_reason"] = "provider_failed"
	} else if mutation.Kind == SD2LedgerPollMutationProviderUnresolved && mutation.TaskStatus == model.TaskStatusFailure {
		updates["fail_reason"] = mutation.SafeErrorCode
	}
	result := tx.Model(&model.Task{}).
		Where("id = ? AND status = ?", task.ID, mutation.ExpectedStatus).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskSubmissionCASLost
	}
	return nil
}
