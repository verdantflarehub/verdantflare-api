package wxmaasseedance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	apiKey      string
	baseURL     string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	if info == nil || info.ChannelMeta == nil {
		return
	}
	a.ChannelType = info.ChannelType
	a.apiKey = info.ApiKey
	a.baseURL = info.ChannelBaseUrl
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	if !service.SD2SubmissionLedgerEnabled() {
		return service.TaskErrorWrapperLocal(fmt.Errorf("the SD2 submission ledger is disabled"), "submission_ledger_disabled", http.StatusServiceUnavailable)
	}
	if info != nil && info.ChannelMeta != nil && info.ChannelIsMultiKey {
		return service.TaskErrorWrapperLocal(fmt.Errorf("selected video channel does not support multi-key credentials"), "invalid_channel_config", http.StatusBadRequest)
	}
	if info != nil && info.TaskRelayInfo != nil && info.Action != "" && info.Action != constant.TaskActionGenerate {
		return service.TaskErrorWrapperLocal(fmt.Errorf("action is not supported"), "unsupported_action", http.StatusBadRequest)
	}
	if c.Request != nil && strings.HasSuffix(c.Request.URL.Path, "/remix") {
		return service.TaskErrorWrapperLocal(fmt.Errorf("remix is not supported"), "unsupported_action", http.StatusBadRequest)
	}

	var req submitRequest
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	taskReq, err := normalizeSubmitRequest(req)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if info != nil {
		if info.TaskRelayInfo == nil {
			info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
		}
		info.Action = constant.TaskActionGenerate
	}
	c.Set("task_request", taskReq)
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return buildURL(a.baseURL, createPath), nil
}

func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	if strings.TrimSpace(a.apiKey) == "" {
		return fmt.Errorf("video channel credential is required")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, errors.Wrap(err, "get task request failed")
	}
	body, err := convertToCreateRequest(req, info)
	if err != nil {
		return nil, errors.Wrap(err, "convert request failed")
	}
	data, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	if !service.SD2SubmissionLedgerEnabled() {
		return nil, fmt.Errorf("the SD2 submission ledger is disabled")
	}
	requestURL, err := a.BuildRequestURL(info)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	if err := a.BuildRequestHeader(c, req, info); err != nil {
		return nil, fmt.Errorf("build request header failed: %w", err)
	}
	proxy := ""
	if info != nil && info.ChannelMeta != nil {
		proxy = info.ChannelSetting.Proxy
	}
	client, err := providerHTTPClient(proxy)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, sanitizedProviderTransportError("create", err)
	}
	return resp, nil
}

func (a *TaskAdaptor) DoResponse(_ *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	if resp == nil || resp.Body == nil {
		return "", nil, submissionUnknownError(info, fmt.Errorf("provider returned no response"))
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes+1))
	if err != nil {
		return "", nil, submissionUnknownError(info, fmt.Errorf("read provider response failed"))
	}
	if len(responseBody) > maxProviderResponseBytes {
		return "", nil, submissionUnknownError(info, fmt.Errorf("provider response is too large"))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", nil, submissionUnknownError(info, fmt.Errorf("provider create outcome is unknown"))
	}

	var providerResponse createResponse
	if err := common.Unmarshal(responseBody, &providerResponse); err != nil {
		return "", nil, submissionUnknownError(info, fmt.Errorf("provider response is invalid"))
	}
	providerResponse.TaskID = strings.TrimSpace(providerResponse.TaskID)
	if err := validateUpstreamTaskID(providerResponse.TaskID); err != nil {
		return "", nil, submissionUnknownError(info, fmt.Errorf("provider response has invalid task_id"))
	}

	snapshot, err := common.Marshal(createSnapshot{
		SchemaVersion:  service.SD2TaskResultSnapshotVersion,
		ProviderStatus: "queued",
		ArchiveState:   service.SD2ArchiveStateNone,
	})
	if err != nil {
		return "", nil, submissionUnknownError(info, fmt.Errorf("create response snapshot failed"))
	}
	return providerResponse.TaskID, snapshot, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	rawTaskID, exists := body["task_id"]
	taskID, ok := rawTaskID.(string)
	if !exists || !ok {
		return nil, fmt.Errorf("task_id must be a string")
	}
	taskID = strings.TrimSpace(taskID)
	if err := validateUpstreamTaskID(taskID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("channel key is required")
	}

	requestURL := buildURL(baseURL, queryPathPrefix+url.PathEscape(taskID))
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client, err := providerHTTPClient(proxy)
	if err != nil {
		return nil, err
	}
	if client.Timeout <= 0 || client.Timeout > providerQueryTimeout {
		client.Timeout = providerQueryTimeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, sanitizedProviderTransportError("query", err)
	}
	return resp, nil
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	if len(respBody) > maxProviderResponseBytes {
		return nil, fmt.Errorf("query response is too large")
	}
	var response queryResponse
	if err := common.Unmarshal(respBody, &response); err != nil {
		return nil, errors.Wrap(err, "unmarshal query response failed")
	}
	response.TaskID = strings.TrimSpace(response.TaskID)
	response.ID = strings.TrimSpace(response.ID)
	if response.TaskID != "" && response.ID != "" && response.TaskID != response.ID {
		return nil, fmt.Errorf("query response contains conflicting task identifiers")
	}
	taskID := response.TaskID
	if taskID == "" {
		taskID = response.ID
	}
	if err := validateUpstreamTaskID(taskID); err != nil {
		return nil, fmt.Errorf("query response has invalid task_id")
	}
	if response.Usage != nil {
		if response.Usage.CompletionTokens < 0 || response.Usage.TotalTokens < 0 {
			return nil, fmt.Errorf("query response has invalid usage")
		}
		if response.Usage.TotalTokens < response.Usage.CompletionTokens {
			return nil, fmt.Errorf("query response has inconsistent usage")
		}
	}
	if err := validateQueryMetadata(&response); err != nil {
		return nil, err
	}

	taskInfo := &relaycommon.TaskInfo{Code: 0, TaskID: taskID}
	if response.Usage != nil {
		taskInfo.CompletionTokens = response.Usage.CompletionTokens
		taskInfo.TotalTokens = response.Usage.TotalTokens
	}
	taskInfo.Seed = response.Seed
	taskInfo.Resolution = strings.TrimSpace(response.Resolution)
	taskInfo.Duration = response.Duration
	taskInfo.Ratio = strings.TrimSpace(response.Ratio)
	taskInfo.FramesPerSecond = response.FramesPerSecond
	if taskInfo.FramesPerSecond == 0 {
		taskInfo.FramesPerSecond = response.Framespersecond
	}
	taskInfo.GenerateAudio = response.GenerateAudio
	taskInfo.ProviderCreatedAt = response.CreatedAt
	taskInfo.ProviderUpdatedAt = response.UpdatedAt

	switch strings.ToLower(strings.TrimSpace(response.Status)) {
	case "queued":
		taskInfo.Status = model.TaskStatusQueued
		taskInfo.Progress = taskcommon.ProgressQueued
	case "running":
		taskInfo.Status = model.TaskStatusInProgress
		taskInfo.Progress = taskcommon.ProgressInProgress
	case "succeeded":
		videoURL := strings.TrimSpace(response.Content.VideoURL)
		if err := validateMediaURL(videoURL); err != nil {
			return nil, fmt.Errorf("succeeded task has invalid video_url")
		}
		taskInfo.Status = model.TaskStatusSuccess
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Url = videoURL
	case "failed":
		taskInfo.Status = model.TaskStatusFailure
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Reason = "provider_failed"
	case "expired":
		taskInfo.Status = model.TaskStatusUnknown
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Reason = "provider_expired"
	case "cancelled":
		taskInfo.Status = model.TaskStatusUnknown
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Reason = "provider_cancelled"
	default:
		return nil, fmt.Errorf("query response has unknown provider status")
	}
	return taskInfo, nil
}

func validateQueryMetadata(response *queryResponse) error {
	if response == nil {
		return fmt.Errorf("query response is required")
	}
	response.Resolution = strings.TrimSpace(response.Resolution)
	if response.Resolution != "" {
		if !strings.EqualFold(response.Resolution, defaultResolution) {
			return fmt.Errorf("query response has unexpected resolution")
		}
		response.Resolution = defaultResolution
	}
	response.Ratio = strings.TrimSpace(response.Ratio)
	if response.Ratio != "" {
		if _, ok := allowedRatios[response.Ratio]; !ok {
			return fmt.Errorf("query response has unexpected ratio")
		}
	}
	if response.Duration != 0 && (response.Duration < minDuration || response.Duration > maxDuration) {
		return fmt.Errorf("query response has invalid duration")
	}
	if response.FramesPerSecond != 0 && response.Framespersecond != 0 && response.FramesPerSecond != response.Framespersecond {
		return fmt.Errorf("query response contains conflicting frame rates")
	}
	framesPerSecond := response.FramesPerSecond
	if framesPerSecond == 0 {
		framesPerSecond = response.Framespersecond
	}
	if framesPerSecond < 0 || framesPerSecond > 240 {
		return fmt.Errorf("query response has invalid frame rate")
	}
	response.FramesPerSecond = framesPerSecond
	if response.CreatedAt < 0 || response.UpdatedAt < 0 ||
		(response.CreatedAt > 0 && response.UpdatedAt > 0 && response.UpdatedAt < response.CreatedAt) {
		return fmt.Errorf("query response has invalid timestamps")
	}
	return nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	video := dto.NewOpenAIVideo()
	video.ID = task.TaskID
	video.TaskID = task.TaskID
	video.Status = task.Status.ToVideoStatus()
	video.SetProgressStr(task.Progress)
	video.CreatedAt = task.CreatedAt
	if task.FinishTime > 0 {
		video.CompletedAt = task.FinishTime
	} else if task.UpdatedAt > 0 {
		video.CompletedAt = task.UpdatedAt
	}
	video.Model = task.Properties.OriginModelName
	if video.Model == "" {
		video.Model = PublicModel
	}

	if task.Status == model.TaskStatusSuccess {
		video.SetMetadata("url", taskcommon.BuildProxyURL(task.TaskID))
	}
	if metadata, ok := parseStoredTaskMetadata(task.Data); ok {
		setMetadataIfPresent(video, "ratio", metadata.Ratio)
		setMetadataIfPresent(video, "duration", metadata.Duration)
		setMetadataIfPresent(video, "resolution", metadata.Resolution)
		setMetadataIfPresent(video, "framespersecond", metadata.FramesPerSecond)
		if metadata.Seed != nil {
			video.SetMetadata("seed", *metadata.Seed)
		}
		if metadata.GenerateAudio != nil {
			video.SetMetadata("generate_audio", *metadata.GenerateAudio)
		}
	}
	if task.Status == model.TaskStatusFailure {
		video.Error = &dto.OpenAIVideoError{
			Code:    "provider_failed",
			Message: "video generation failed",
		}
	}
	return common.MarshalNoHTMLEscape(video)
}

func providerHTTPClient(proxy string) (*http.Client, error) {
	baseClient, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("provider HTTP client configuration failed")
	}
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client, nil
}

func sanitizedProviderTransportError(action string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("provider %s request failed", action)
	}
}

func buildURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return baseURL + path
}

func validateUpstreamTaskID(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("task_id is required")
	}
	if len(taskID) > 256 {
		return fmt.Errorf("task_id is too long")
	}
	if taskID == "." || taskID == ".." {
		return fmt.Errorf("task_id contains invalid path segment")
	}
	for _, char := range taskID {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("task_id contains invalid characters")
	}
	return nil
}

func submissionUnknownError(info *relaycommon.RelayInfo, err error) *dto.TaskError {
	taskErr := service.TaskErrorWrapper(err, "video_submission_unknown", http.StatusBadGateway)
	data := map[string]any{"submission_state": "UNKNOWN"}
	if info != nil && info.TaskRelayInfo != nil && strings.TrimSpace(info.PublicTaskID) != "" {
		data["task_id"] = info.PublicTaskID
	}
	taskErr.Data = data
	return taskErr
}

func parseStoredTaskMetadata(raw []byte) (storedTaskMetadata, bool) {
	var response queryResponse
	if err := common.Unmarshal(raw, &response); err != nil {
		return storedTaskMetadata{}, false
	}
	framesPerSecond := response.FramesPerSecond
	if framesPerSecond == 0 {
		framesPerSecond = response.Framespersecond
	}
	return storedTaskMetadata{
		Seed:            response.Seed,
		Resolution:      response.Resolution,
		Ratio:           response.Ratio,
		Duration:        response.Duration,
		FramesPerSecond: framesPerSecond,
		GenerateAudio:   response.GenerateAudio,
	}, true
}

func setMetadataIfPresent(video *dto.OpenAIVideo, key string, value any) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			video.SetMetadata(key, typed)
		}
	case int:
		if typed != 0 {
			video.SetMetadata(key, typed)
		}
	case int64:
		if typed != 0 {
			video.SetMetadata(key, typed)
		}
	}
}
