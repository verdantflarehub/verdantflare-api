package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const (
	sd2SendFenceTimeout      = 2 * time.Minute
	sd2ProviderCreateTimeout = 90 * time.Second
)

func relaySD2Task(c *gin.Context, relayInfo *relaycommon.RelayInfo) {
	if isSD2RemixRequest(c.Request.URL.Path, relayInfo.Action) {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("SD2 remix is not enabled"), "unsupported_action", http.StatusBadRequest))
		return
	}
	clientRequestID := c.GetString(middleware.ContextKeySD2ClientRequestID)
	requestDigest := c.GetString(middleware.ContextKeySD2RequestDigest)
	digestVersion := c.GetString(middleware.ContextKeySD2DigestVersion)
	normalizedValue, exists := c.Get(middleware.ContextKeySD2Normalized)
	normalized, ok := normalizedValue.(*service.SD2NormalizedCreate)
	if clientRequestID == "" || requestDigest == "" || digestVersion == "" || !exists || !ok || normalized == nil {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("SD2 idempotency context is missing"), "missing_idempotency_key", http.StatusBadRequest))
		return
	}

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}
	var selectedChannel *model.Channel
	var channelType int
	var eligibilityErr *dto.TaskError
	for selectionAttempt := 0; selectionAttempt < 2; selectionAttempt++ {
		var channelErr *types.NewAPIError
		selectedChannel, channelErr = getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			eligibilityErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
			break
		}
		addUsedChannel(c, selectedChannel.Id)
		channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
		eligibilityErr = sd2ChannelEligibility(relayInfo.UserId, channelType, common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey), normalized)
		if eligibilityErr == nil {
			break
		}
		// Capability and rollout filtering is safe before T1/T2. Mark the
		// current channel used, then ask the existing selector for one other
		// candidate. There are only two SD2 providers in this release.
		if selectionAttempt == 0 {
			relayInfo.InitChannelMeta(c)
			retryParam.IncreaseRetry()
		}
	}
	if eligibilityErr != nil {
		respondTaskError(c, eligibilityErr)
		return
	}
	relayInfo.InitChannelMeta(c)
	providerName := sd2ProviderName(channelType)
	deliveryProbe := &model.TaskSubmission{
		UserID:           relayInfo.UserId,
		ChannelID:        relayInfo.ChannelId,
		ChannelType:      channelType,
		Provider:         providerName,
		LogicalAccountID: fmt.Sprintf("channel:%d", relayInfo.ChannelId),
	}
	if err := service.ValidateSD2ResultDeliveryReady(deliveryProbe); err != nil {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("private video result delivery is not configured"), "result_delivery_unavailable", http.StatusServiceUnavailable))
		return
	}

	prepared, taskErr := relay.PrepareTaskSubmit(c, relayInfo)
	if taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}
	if prepared.Quota <= 0 || relayInfo.PriceData.FreeModel {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("verdantflare-sd2 requires a positive fixed product price"), "model_price_error", http.StatusBadRequest))
		return
	}
	if relayInfo.ChannelIsMultiKey {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("SD2 submission channels must use exactly one credential"), "invalid_channel_config", http.StatusServiceUnavailable))
		return
	}

	baseOrigin, err := sd2CanonicalBaseOrigin(relayInfo.ChannelBaseUrl, channelType)
	if err != nil {
		respondTaskError(c, service.TaskErrorWrapperLocal(err, "invalid_channel_config", http.StatusServiceUnavailable))
		return
	}
	credentialFingerprint := service.SD2ChannelCredentialFingerprint(relayInfo.ApiKey)
	if credentialFingerprint == "" {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("SD2 credential identity is unavailable"), "submission_identity_unavailable", http.StatusServiceUnavailable))
		return
	}
	provider, providerCost := sd2ProviderCostSnapshot(channelType, normalized, relayInfo.UpstreamModelName)
	if provider == "" {
		respondTaskError(c, service.TaskErrorWrapperLocal(errors.New("unsupported SD2 provider"), "invalid_sd2_channel", http.StatusServiceUnavailable))
		return
	}
	providerRequestDigest := common.GenerateHMAC("sd2-provider-request-v1\x00" + string(prepared.RequestBody))
	requestSummary := service.SubmissionRequestSummary{
		SchemaVersion: 1,
		Action:        constant.TaskActionGenerate,
		Duration:      normalized.Duration,
		Ratio:         normalized.Ratio,
		Resolution:    normalized.Resolution,
		ImageCount:    normalized.ImageCount,
		VideoCount:    normalized.VideoCount,
		AudioCount:    normalized.AudioCount,
		GenerateAudio: normalized.GenerateAudio,
		Watermark:     normalized.Watermark,
		Seed:          normalized.Seed,
	}
	publicPrice := service.SubmissionPublicPriceSnapshot{
		SchemaVersion: 1,
		PriceVersion:  fmt.Sprintf("fixed-per-call-v1:%s:%d", service.SD2OriginModel, prepared.Quota),
		ModelName:     service.SD2OriginModel,
		ProductQuota:  prepared.Quota,
		GroupRatio:    strconv.FormatFloat(relayInfo.PriceData.GroupRatioInfo.GroupRatio, 'f', -1, 64),
		Currency:      "QUOTA",
	}

	fundingCandidates, err := service.SubmissionFundingCandidates(relayInfo.UserId, relayInfo.UserSetting.BillingPreference)
	if err != nil {
		respondTaskError(c, service.TaskErrorWrapperLocal(err, "billing_source_error", http.StatusInternalServerError))
		return
	}
	var submission *model.TaskSubmission
	var created bool
	var prepareErr error
	for _, fundingSource := range fundingCandidates {
		submission, created, prepareErr = service.PrepareTaskSubmission(service.PrepareTaskSubmissionInput{
			UserID:                relayInfo.UserId,
			ClientRequestID:       clientRequestID,
			RequestDigest:         requestDigest,
			DigestVersion:         digestVersion,
			RequestSummary:        requestSummary,
			ProviderRequestDigest: providerRequestDigest,
			PublicTaskID:          relayInfo.PublicTaskID,
			ChannelID:             relayInfo.ChannelId,
			ChannelType:           channelType,
			CanonicalBaseOrigin:   baseOrigin,
			Provider:              provider,
			LogicalAccountID:      fmt.Sprintf("channel:%d", relayInfo.ChannelId),
			CredentialRef:         fmt.Sprintf("channel:%d:key:%s", relayInfo.ChannelId, credentialFingerprint[:16]),
			CredentialFingerprint: credentialFingerprint,
			OriginModelName:       service.SD2OriginModel,
			UpstreamModelName:     relayInfo.UpstreamModelName,
			ReservedQuota:         prepared.Quota,
			TokenID:               relayInfo.TokenId,
			FundingSource:         fundingSource,
			PublicPriceSnapshot:   publicPrice,
			ProviderCostSnapshot:  providerCost,
		})
		if prepareErr == nil || !errors.Is(prepareErr, service.ErrTaskSubmissionInsufficientQuota) {
			break
		}
	}
	if prepareErr != nil {
		switch {
		case errors.Is(prepareErr, service.ErrTaskSubmissionIdempotencyConflict):
			writeSD2ControllerError(c, http.StatusConflict, "idempotency_conflict", "the idempotency key was already used for a different request", clientRequestID)
		case errors.Is(prepareErr, service.ErrTaskSubmissionInsufficientQuota):
			writeSD2ControllerError(c, http.StatusForbidden, "insufficient_quota", "insufficient quota for this video submission", clientRequestID)
		case errors.Is(prepareErr, service.ErrTaskSubmissionBatchQuotaUnsafe):
			writeSD2ControllerError(c, http.StatusServiceUnavailable, "submission_billing_unsafe", "video creation is disabled until database-authoritative quota mode is enabled", clientRequestID)
		case service.IsSD2ChannelSafetyError(prepareErr):
			writeSD2ControllerError(c, http.StatusTooManyRequests, "channel_safety_gate_rejected", "the selected video channel is not accepting new paid tasks", clientRequestID)
		default:
			writeSD2ControllerError(c, http.StatusServiceUnavailable, "submission_prepare_failed", "the video submission could not be prepared safely", clientRequestID)
		}
		return
	}
	if !created {
		writeSD2ControllerReplay(c, submission)
		return
	}

	claimed, won, err := service.AcquireSubmissionSend(submission.ID, submission.Version, time.Now().Add(sd2SendFenceTimeout).Unix())
	if service.IsSD2ChannelSafetyError(err) {
		writeSD2ControllerError(c, http.StatusTooManyRequests, "channel_safety_gate_rejected", "the selected video channel is not accepting new paid tasks", clientRequestID)
		return
	}
	if err != nil || !won || claimed == nil {
		writeSD2ControllerError(c, http.StatusConflict, "submission_in_progress", "this video submission is already being processed", clientRequestID)
		return
	}
	relayInfo.PublicTaskID = claimed.PublicTaskID
	var result *relay.TaskSubmitResult
	var sendErr *dto.TaskError
	func() {
		// Keep the single provider POST strictly inside the durable SENDING
		// lease. The shorter request deadline leaves recovery time to record an
		// UNKNOWN outcome and, critically, never authorizes a second POST.
		sendCtx, cancel := context.WithTimeout(c.Request.Context(), sd2ProviderCreateTimeout)
		defer cancel()
		originalRequest := c.Request
		c.Request = c.Request.Clone(sendCtx)
		defer func() { c.Request = originalRequest }()
		result, sendErr = relay.SendPreparedTask(c, relayInfo, prepared)
	}()
	if sendErr != nil || result == nil || strings.TrimSpace(result.UpstreamTaskID) == "" {
		status, responseDigest := sd2ProviderEvidence(result)
		_, _ = service.MarkSubmissionUnknown(claimed.ID, claimed.Version, "video_submission_unknown", "the provider submission outcome requires reconciliation", status, responseDigest)
		writeSD2ControllerError(c, http.StatusConflict, "submission_unknown", "the provider submission outcome is unknown and will not be retried", clientRequestID)
		return
	}

	initialSnapshot := service.NewSD2TaskResultSnapshot()
	initialSnapshot.ProviderStatus = "queued"
	initialData, err := service.EncodeSD2TaskResultSnapshot(initialSnapshot)
	if err != nil {
		status, responseDigest := sd2ProviderEvidence(result)
		_, _ = service.MarkSubmissionUnknown(claimed.ID, claimed.Version, "local_snapshot_failed", "the provider task could not be committed safely", status, responseDigest)
		writeSD2ControllerError(c, http.StatusConflict, "submission_unknown", "the provider task exists but local confirmation requires reconciliation", clientRequestID)
		return
	}
	task := model.InitTask(result.Platform, relayInfo)
	task.Action = constant.TaskActionGenerate
	task.Data = initialData
	task.Quota = prepared.Quota
	task.PrivateData.NodeName = common.NodeName
	task.PrivateData.BillingSource = claimed.BillingSource
	task.PrivateData.BillingContext = &model.TaskBillingContext{
		ModelPrice:      relayInfo.PriceData.ModelPrice,
		GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
		ModelRatio:      relayInfo.PriceData.ModelRatio,
		OriginModelName: service.SD2OriginModel,
		PerCallBilling:  true,
	}

	var committed *model.TaskSubmission
	var commitErr error
	for attempt := 0; attempt < 3; attempt++ {
		committed, commitErr = service.CommitSubmission(claimed.ID, claimed.Version, model.TaskSubmissionStateSending, result.UpstreamTaskID, task, "api")
		if commitErr == nil {
			break
		}
	}
	if commitErr != nil || committed == nil {
		status, responseDigest := sd2ProviderEvidence(result)
		_, _ = service.MarkSubmissionUnknown(claimed.ID, claimed.Version, "local_commit_failed", "the provider task could not be committed safely", status, responseDigest)
		writeSD2ControllerError(c, http.StatusConflict, "submission_unknown", "the provider task exists but local confirmation requires reconciliation", clientRequestID)
		return
	}

	relayInfo.FinalPreConsumedQuota = committed.ReservedQuota
	relayInfo.BillingSource = committed.BillingSource
	if committed.SubscriptionID != nil {
		relayInfo.SubscriptionId = *committed.SubscriptionID
	}
	service.LogTaskConsumption(c, relayInfo)
	video := dto.NewOpenAIVideo()
	video.ID = committed.PublicTaskID
	video.TaskID = committed.PublicTaskID
	video.CreatedAt = committed.CreatedAt
	video.Model = service.SD2OriginModel
	c.JSON(http.StatusOK, video)
}

func isSD2RemixRequest(path string, action string) bool {
	return strings.HasSuffix(path, "/remix") || action == constant.TaskActionRemix
}

func sd2ProviderCostSnapshot(channelType int, normalized *service.SD2NormalizedCreate, upstreamModel string) (string, service.SubmissionProviderCostSnapshot) {
	snapshot := service.SubmissionProviderCostSnapshot{
		SchemaVersion:     1,
		UpstreamModelName: upstreamModel,
		Resolution:        normalized.Resolution,
		HasVideo:          normalized.VideoCount > 0,
		Duration:          normalized.Duration,
		ImageCount:        normalized.ImageCount,
		VideoCount:        normalized.VideoCount,
		AudioCount:        normalized.AudioCount,
	}
	switch channelType {
	case constant.ChannelTypeWxmaasSeedance:
		snapshot.Provider = "wxmaas-seedance"
		snapshot.ProviderPriceKey = "wxmaas/doubao-seedance-2.0/2026-08-05"
		snapshot.ProviderPriceVersion = "2026-08-05"
		if snapshot.HasVideo {
			snapshot.UnitPricePerMillionCNY = "28"
		} else {
			snapshot.UnitPricePerMillionCNY = "46"
		}
		snapshot.RoundingRule = "ROUND_HALF_UP"
		return snapshot.Provider, snapshot
	case constant.ChannelTypeJDSeedance:
		snapshot.Provider = "jd-seedance"
		snapshot.ProviderPriceKey = "jd-seedance/manual-reconcile"
		snapshot.ProviderPriceVersion = "manual"
		snapshot.UnitPricePerMillionCNY = "0"
		snapshot.RoundingRule = "MANUAL_RECONCILE"
		return snapshot.Provider, snapshot
	default:
		return "", snapshot
	}
}

func sd2ProviderName(channelType int) string {
	switch channelType {
	case constant.ChannelTypeWxmaasSeedance:
		return "wxmaas-seedance"
	case constant.ChannelTypeJDSeedance:
		return "jd-seedance"
	default:
		return ""
	}
}

func sd2ChannelEligibility(userID int, channelType int, multiKey bool, normalized *service.SD2NormalizedCreate) *dto.TaskError {
	if channelType != constant.ChannelTypeJDSeedance && channelType != constant.ChannelTypeWxmaasSeedance {
		return service.TaskErrorWrapperLocal(errors.New("selected channel is not an SD2 provider"), "invalid_sd2_channel", http.StatusServiceUnavailable)
	}
	if multiKey {
		return service.TaskErrorWrapperLocal(errors.New("the selected SD2 channel has an invalid credential layout"), "invalid_channel_config", http.StatusServiceUnavailable)
	}
	if channelType != constant.ChannelTypeWxmaasSeedance {
		if normalized != nil && normalized.Seed != service.SD2DefaultSeed {
			return service.TaskErrorWrapperLocal(errors.New("the selected SD2 channel does not support deterministic seed control"), "provider_capability_mismatch", http.StatusServiceUnavailable)
		}
		return nil
	}
	if normalized == nil || normalized.Duration < 4 || normalized.Duration > 15 {
		return service.TaskErrorWrapperLocal(errors.New("the selected SD2 channel does not support this duration"), "provider_capability_mismatch", http.StatusServiceUnavailable)
	}
	if !sd2WxmaasCreateEnabled() {
		return service.TaskErrorWrapperLocal(errors.New("the selected SD2 channel is disabled for new tasks"), "provider_create_disabled", http.StatusServiceUnavailable)
	}
	if !model.IsAdmin(userID) {
		return service.TaskErrorWrapperLocal(errors.New("the selected SD2 channel is limited to staff traffic"), "provider_staff_only", http.StatusForbidden)
	}
	return nil
}

func sd2CanonicalBaseOrigin(baseURL string, channelType int) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = constant.ChannelBaseURLs[channelType]
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("SD2 channel base URL must be a credential-free HTTPS origin")
	}
	return "https://" + strings.ToLower(parsed.Host), nil
}

func sd2WxmaasCreateEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("WXMAAS_SD2_CREATE_ENABLED")))
	return err == nil && enabled
}

func sd2ProviderEvidence(result *relay.TaskSubmitResult) (*int, string) {
	if result == nil {
		return nil, ""
	}
	var status *int
	if result.ProviderHTTPStatus > 0 {
		value := result.ProviderHTTPStatus
		status = &value
	}
	return status, result.ResponseDigest
}

func writeSD2ControllerReplay(c *gin.Context, submission *model.TaskSubmission) {
	if submission == nil {
		writeSD2ControllerError(c, http.StatusConflict, "submission_in_progress", "this video submission is already being processed", "")
		return
	}
	switch submission.State {
	case model.TaskSubmissionStateConfirmed:
		video := dto.NewOpenAIVideo()
		video.ID = submission.PublicTaskID
		video.TaskID = submission.PublicTaskID
		video.CreatedAt = submission.CreatedAt
		video.Model = service.SD2OriginModel
		c.JSON(http.StatusOK, video)
	case model.TaskSubmissionStateUnknown:
		writeSD2ControllerError(c, http.StatusConflict, "submission_unknown", "the provider submission outcome is unknown and requires reconciliation", submission.ClientRequestID)
	case model.TaskSubmissionStateRejected:
		writeSD2ControllerError(c, http.StatusConflict, "submission_rejected", "the original video submission was rejected", submission.ClientRequestID)
	default:
		writeSD2ControllerError(c, http.StatusConflict, "submission_in_progress", "this video submission is already being processed", submission.ClientRequestID)
	}
}

func writeSD2ControllerError(c *gin.Context, status int, code string, message string, clientRequestID string) {
	c.JSON(status, &dto.TaskError{
		Code:       code,
		Message:    message,
		StatusCode: status,
		LocalError: true,
		Data: map[string]any{
			"client_request_id": clientRequestID,
			"submission_url":    "/v1/video-submissions/" + clientRequestID,
		},
	})
}
