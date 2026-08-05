package controller

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type adminTaskSubmissionView struct {
	ID                int64                                 `json:"id"`
	CreatedAt         int64                                 `json:"created_at"`
	UpdatedAt         int64                                 `json:"updated_at"`
	Version           int64                                 `json:"version"`
	UserID            int                                   `json:"user_id"`
	ClientRequestID   string                                `json:"client_request_id"`
	PublicTaskID      string                                `json:"public_task_id"`
	State             model.TaskSubmissionState             `json:"state"`
	PollState         model.TaskSubmissionPollState         `json:"poll_state"`
	BillingState      model.TaskSubmissionBillingState      `json:"billing_state"`
	ProviderCostState model.TaskSubmissionProviderCostState `json:"provider_cost_state"`
	ArchiveState      model.TaskSubmissionArchiveState      `json:"archive_state"`
	Provider          string                                `json:"provider"`
	ChannelID         int                                   `json:"channel_id"`
	ChannelType       int                                   `json:"channel_type"`
	OriginModelName   string                                `json:"origin_model_name"`
	ReservedQuota     int                                   `json:"reserved_quota"`
	BillingSource     string                                `json:"billing_source"`
	ProviderStatus    string                                `json:"provider_status,omitempty"`
	ErrorCode         string                                `json:"error_code,omitempty"`
	ErrorMessage      string                                `json:"error_message,omitempty"`
	SendAttempts      int                                   `json:"send_attempts"`
	PollAttempts      int                                   `json:"poll_attempts"`
}

type adminTaskSubmissionDetail struct {
	adminTaskSubmissionView
	RequestSummary       string                                    `json:"request_summary,omitempty"`
	PublicPriceSnapshot  string                                    `json:"public_price_snapshot,omitempty"`
	ProviderCostSnapshot string                                    `json:"provider_cost_snapshot,omitempty"`
	UpstreamTaskID       string                                    `json:"upstream_task_id,omitempty"`
	ProviderRequestID    string                                    `json:"provider_request_id,omitempty"`
	BillingEntries       []*model.TaskSubmissionBillingEntry       `json:"billing_entries"`
	Reviews              []adminTaskSubmissionReconciliationReview `json:"reconciliation_reviews"`
}

type adminTaskSubmissionReconciliationReview struct {
	ID                     int64  `json:"id"`
	ExpectedVersion        int64  `json:"expected_version"`
	Decision               string `json:"decision"`
	UpstreamTaskID         string `json:"upstream_task_id,omitempty"`
	AdminID                int    `json:"admin_id"`
	Reason                 string `json:"reason"`
	EvidenceRecorded       bool   `json:"evidence_recorded"`
	ProviderRequestID      string `json:"provider_request_id,omitempty"`
	StrongCorrelation      bool   `json:"strong_correlation"`
	Status                 string `json:"status"`
	ErrorCode              string `json:"error_code,omitempty"`
	SubmissionVersionAfter *int64 `json:"submission_version_after,omitempty"`
	CreatedAt              int64  `json:"created_at"`
}

type adminTaskSubmissionReconcileRequest struct {
	ExpectedVersion   int64  `json:"expected_version"`
	Decision          string `json:"decision"`
	Evidence          string `json:"evidence"`
	Reason            string `json:"reason"`
	UpstreamTaskID    string `json:"upstream_task_id"`
	ProviderRequestID string `json:"provider_request_id"`
}

var taskSubmissionPrivateURLPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

func AdminListTaskSubmissions(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	filter, err := adminTaskSubmissionFilter(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid task submission filter"})
		return
	}
	filter.Offset = pageInfo.GetStartIdx()
	filter.Limit = pageInfo.GetPageSize()
	items, err := model.ListTaskSubmissions(filter)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	total, err := model.CountTaskSubmissions(filter)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	views := make([]adminTaskSubmissionView, 0, len(items))
	for _, item := range items {
		views = append(views, newAdminTaskSubmissionView(item))
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(views)
	common.ApiSuccess(c, pageInfo)
}

func AdminGetTaskSubmission(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid task submission id"})
		return
	}
	submission, err := model.GetTaskSubmissionByID(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if submission == nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "task submission not found"})
		return
	}
	entries, err := model.ListTaskSubmissionBillingEntries(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	reviews, err := model.ListTaskSubmissionReconciliationReviews(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	detail := adminTaskSubmissionDetail{
		adminTaskSubmissionView: newAdminTaskSubmissionView(submission),
		RequestSummary:          redactTaskSubmissionSnapshot(submission.RequestSummary),
		PublicPriceSnapshot:     redactTaskSubmissionSnapshot(submission.PublicPriceSnapshot),
		ProviderCostSnapshot:    redactTaskSubmissionSnapshot(submission.ProviderCostSnapshot),
		ProviderRequestID:       submission.ProviderRequestID,
		BillingEntries:          entries,
		Reviews:                 make([]adminTaskSubmissionReconciliationReview, 0, len(reviews)),
	}
	if submission.UpstreamTaskID != nil {
		detail.UpstreamTaskID = *submission.UpstreamTaskID
	}
	for _, review := range reviews {
		detail.Reviews = append(detail.Reviews, adminTaskSubmissionReconciliationReview{
			ID:                     review.ID,
			ExpectedVersion:        review.ExpectedVersion,
			Decision:               review.Decision,
			UpstreamTaskID:         review.UpstreamTaskID,
			AdminID:                review.AdminID,
			Reason:                 redactTaskSubmissionText(review.Reason),
			EvidenceRecorded:       strings.TrimSpace(review.Evidence) != "",
			ProviderRequestID:      review.ProviderRequestID,
			StrongCorrelation:      review.StrongCorrelation,
			Status:                 review.Status,
			ErrorCode:              review.SafeErrorCode,
			SubmissionVersionAfter: review.SubmissionVersionAfter,
			CreatedAt:              review.CreatedAt,
		})
	}
	common.ApiSuccess(c, detail)
}

func AdminReconcileTaskSubmission(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid task submission id"})
		return
	}
	var request adminTaskSubmissionReconcileRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid reconciliation request"})
		return
	}
	result, err := service.ReconcileTaskSubmissionAdmin(service.AdminTaskSubmissionReconcileInput{
		Context:           c.Request.Context(),
		SubmissionID:      id,
		ExpectedVersion:   request.ExpectedVersion,
		Decision:          request.Decision,
		Evidence:          request.Evidence,
		Reason:            request.Reason,
		UpstreamTaskID:    request.UpstreamTaskID,
		ProviderRequestID: request.ProviderRequestID,
		AdminID:           c.GetInt("id"),
	})
	if errors.Is(err, service.ErrTaskSubmissionEvidenceRequired) && result != nil {
		c.JSON(http.StatusAccepted, gin.H{
			"success": true,
			"message": "review recorded; verified structured provider evidence is required before execution",
			"data":    gin.H{"pending_review": true, "requires_evidence": true},
		})
		return
	}
	if errors.Is(err, service.ErrTaskSubmissionReviewPending) && result != nil {
		c.JSON(http.StatusAccepted, gin.H{
			"success": true,
			"message": "a second distinct administrator must approve this bind",
			"data":    gin.H{"pending_review": true, "reviewer_count": result.ReviewerCount},
		})
		return
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, service.ErrTaskSubmissionReconcileInput) {
			status = http.StatusBadRequest
		} else if errors.Is(err, service.ErrTaskSubmissionNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"success": false, "message": err.Error()})
		return
	}
	common.ApiSuccess(c, gin.H{
		"pending_review": false,
		"reviewer_count": result.ReviewerCount,
		"submission":     newAdminTaskSubmissionView(result.Submission),
	})
}

func adminTaskSubmissionFilter(c *gin.Context) (model.TaskSubmissionListFilter, error) {
	filter := model.TaskSubmissionListFilter{
		ClientRequestID:   strings.TrimSpace(c.Query("client_request_id")),
		PublicTaskID:      strings.TrimSpace(c.Query("public_task_id")),
		ProviderRequestID: strings.TrimSpace(c.Query("provider_request_id")),
		UpstreamTaskID:    strings.TrimSpace(c.Query("upstream_task_id")),
		Provider:          strings.TrimSpace(c.Query("provider")),
		State:             model.TaskSubmissionState(strings.TrimSpace(c.Query("state"))),
		ErrorCode:         strings.TrimSpace(c.Query("error")),
	}
	var err error
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		filter.UserID, err = strconv.Atoi(raw)
		if err != nil || filter.UserID <= 0 {
			return filter, errors.New("invalid user_id")
		}
	}
	if raw := strings.TrimSpace(c.Query("channel_id")); raw != "" {
		filter.ChannelID, err = strconv.Atoi(raw)
		if err != nil || filter.ChannelID <= 0 {
			return filter, errors.New("invalid channel_id")
		}
	}
	if raw := strings.TrimSpace(c.Query("start_timestamp")); raw != "" {
		filter.CreatedAfter, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.CreatedAfter <= 0 {
			return filter, errors.New("invalid start_timestamp")
		}
	}
	if raw := strings.TrimSpace(c.Query("end_timestamp")); raw != "" {
		filter.CreatedBefore, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.CreatedBefore <= 0 {
			return filter, errors.New("invalid end_timestamp")
		}
	}
	return filter, nil
}

func newAdminTaskSubmissionView(submission *model.TaskSubmission) adminTaskSubmissionView {
	if submission == nil {
		return adminTaskSubmissionView{}
	}
	return adminTaskSubmissionView{
		ID:                submission.ID,
		CreatedAt:         submission.CreatedAt,
		UpdatedAt:         submission.UpdatedAt,
		Version:           submission.Version,
		UserID:            submission.UserID,
		ClientRequestID:   submission.ClientRequestID,
		PublicTaskID:      submission.PublicTaskID,
		State:             submission.State,
		PollState:         submission.PollState,
		BillingState:      submission.BillingState,
		ProviderCostState: submission.ProviderCostState,
		ArchiveState:      submission.ArchiveState,
		Provider:          submission.Provider,
		ChannelID:         submission.ChannelID,
		ChannelType:       submission.ChannelType,
		OriginModelName:   submission.OriginModelName,
		ReservedQuota:     submission.ReservedQuota,
		BillingSource:     submission.BillingSource,
		ProviderStatus:    submission.ProviderStatus,
		ErrorCode:         submission.SafeErrorCode,
		ErrorMessage:      submission.SafeErrorMessage,
		SendAttempts:      submission.SendAttempts,
		PollAttempts:      submission.PollAttempts,
	}
}

func redactTaskSubmissionSnapshot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var value any
	if err := common.UnmarshalJsonStr(raw, &value); err != nil {
		return "[redacted malformed snapshot]"
	}
	redacted, err := common.Marshal(redactTaskSubmissionSnapshotValue(value))
	if err != nil {
		return "[redacted snapshot]"
	}
	return string(redacted)
}

func redactTaskSubmissionSnapshotValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, nested := range typed {
			lowerKey := strings.ToLower(key)
			if strings.Contains(lowerKey, "prompt") || strings.Contains(lowerKey, "url") || strings.Contains(lowerKey, "uri") || strings.Contains(lowerKey, "credential") || strings.Contains(lowerKey, "secret") || lowerKey == "key" || lowerKey == "api_key" || lowerKey == "access_token" {
				continue
			}
			clean[key] = redactTaskSubmissionSnapshotValue(nested)
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, nested := range typed {
			clean[index] = redactTaskSubmissionSnapshotValue(nested)
		}
		return clean
	case string:
		return redactTaskSubmissionText(typed)
	default:
		return typed
	}
}

func redactTaskSubmissionText(value string) string {
	return taskSubmissionPrivateURLPattern.ReplaceAllString(value, "[redacted-url]")
}
