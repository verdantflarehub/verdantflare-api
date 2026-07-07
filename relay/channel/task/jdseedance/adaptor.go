package jdseedance

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
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
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	var req submitRequest
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	taskReq, err := normalizeSubmitRequest(req)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if strings.TrimSpace(taskReq.Model) == "" {
		return service.TaskErrorWrapperLocal(fmt.Errorf("model field is required"), "missing_model", http.StatusBadRequest)
	}
	info.Action = constant.TaskActionGenerate
	c.Set("task_request", taskReq)
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return buildURL(a.baseURL, createPath), nil
}

func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
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
	if info.UpstreamModelName == "" {
		info.UpstreamModelName = req.Model
	}

	body, err := convertToCreateRequest(req)
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
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	var jdResp createResponse
	if err := common.Unmarshal(responseBody, &jdResp); err != nil {
		return "", responseBody, service.TaskErrorWrapper(errors.Wrapf(err, "body: %s", responseBody), "unmarshal_response_body_failed", http.StatusBadGateway)
	}
	upstreamTaskID := strings.TrimSpace(common.Interface2String(jdResp.Data))
	if jdResp.Code != 1 || upstreamTaskID == "" {
		msg := whiteLabelUpstreamMessage(jdResp.Msg)
		if msg == "" {
			msg = "video generation create failed"
		}
		return "", responseBody, service.TaskErrorWrapper(fmt.Errorf("%s", msg), "video_generation_create_failed", http.StatusBadGateway)
	}

	video := dto.NewOpenAIVideo()
	video.ID = info.PublicTaskID
	video.TaskID = info.PublicTaskID
	video.CreatedAt = time.Now().Unix()
	video.Model = info.OriginModelName
	c.JSON(http.StatusOK, video)

	return upstreamTaskID, responseBody, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID := strings.TrimSpace(common.Interface2String(body["task_id"]))
	if taskID == "" {
		return nil, fmt.Errorf("invalid task_id")
	}

	payload, err := common.Marshal(queryRequest{TaskID: taskID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, buildURL(baseURL, queryPath), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	var resp queryResponse
	if err := common.Unmarshal(respBody, &resp); err != nil {
		return nil, errors.Wrap(err, "unmarshal query response failed")
	}
	if resp.Code != 1 {
		msg := whiteLabelUpstreamMessage(resp.Msg)
		if msg == "" {
			msg = "video generation query failed"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	var data queryData
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("query response data is empty")
	}
	if err := common.Unmarshal(resp.Data, &data); err != nil {
		return nil, errors.Wrap(err, "unmarshal query data failed")
	}

	taskInfo := &relaycommon.TaskInfo{
		Code:   0,
		TaskID: data.ID,
	}

	switch strings.ToLower(strings.TrimSpace(data.Status)) {
	case "pending", "queued", "submitted":
		taskInfo.Status = model.TaskStatusQueued
		taskInfo.Progress = taskcommon.ProgressQueued
	case "processing", "running":
		taskInfo.Status = model.TaskStatusInProgress
		taskInfo.Progress = "50%"
	case "success", "succeeded", "completed":
		taskInfo.Status = model.TaskStatusSuccess
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Url = extractResultURL(data)
	case "failed", "failure", "canceled", "cancelled":
		taskInfo.Status = model.TaskStatusFailure
		taskInfo.Progress = taskcommon.ProgressComplete
		taskInfo.Reason = failureReason(resp.Msg, data)
	default:
		taskInfo.Status = model.TaskStatusInProgress
		taskInfo.Progress = taskcommon.ProgressInProgress
	}

	return taskInfo, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	video := dto.NewOpenAIVideo()
	video.ID = task.TaskID
	video.TaskID = task.TaskID
	video.Status = task.Status.ToVideoStatus()
	if video.Status == dto.VideoStatusUnknown {
		video.Status = dto.VideoStatusQueued
	}
	video.SetProgressStr(task.Progress)
	video.CreatedAt = task.CreatedAt
	if task.FinishTime > 0 {
		video.CompletedAt = task.FinishTime
	} else if task.UpdatedAt > 0 {
		video.CompletedAt = task.UpdatedAt
	}
	video.Model = task.Properties.OriginModelName
	if video.Model == "" {
		video.Model = ModelJDSeedanceSD
	}
	if url := strings.TrimSpace(task.GetResultURL()); url != "" {
		video.SetMetadata("url", url)
	}

	if data, ok := parseStoredQueryData(task.Data); ok {
		if url := extractResultURL(data); url != "" {
			video.SetMetadata("url", url)
		}
		setMetadataIfNotZero(video, "ratio", data.Ratio)
		setMetadataIfNotZero(video, "duration", data.Duration)
		setMetadataIfNotZero(video, "resolution", data.Resolution)
		setMetadataIfNotZero(video, "framespersecond", data.FramesPerSecond)
		video.SetMetadata("generate_audio", data.GenerateAudio)
		if task.Status == model.TaskStatusFailure {
			video.Error = &dto.OpenAIVideoError{
				Code:    data.Error.Code,
				Message: failureReason("", data),
			}
		}
	} else if task.Status == model.TaskStatusFailure {
		video.Error = &dto.OpenAIVideoError{
			Code:    "task_failed",
			Message: task.FailReason,
		}
	}

	return common.Marshal(video)
}

func buildURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return baseURL + path
}

func whiteLabelUpstreamMessage(message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"JD Seedance", "video generation",
		"JDSeedance", "video generation",
		"JD seedance", "video generation",
		"jd seedance", "video generation",
		"jd-seedance", "video-generation",
		"JD-Seedance", "video-generation",
		"Seedance", "video generation",
		"seedance", "video generation",
		"京东", "upstream",
	)
	return replacer.Replace(msg)
}

func parseStoredQueryData(raw []byte) (queryData, bool) {
	var resp queryResponse
	if err := common.Unmarshal(raw, &resp); err != nil || len(resp.Data) == 0 {
		return queryData{}, false
	}
	var data queryData
	if err := common.Unmarshal(resp.Data, &data); err != nil {
		return queryData{}, false
	}
	return data, true
}

func extractResultURL(data queryData) string {
	if url := extractResultURLFromContent(data.Content); url != "" {
		return url
	}
	if url := strings.TrimSpace(data.VideoURL); url != "" {
		return url
	}
	return strings.TrimSpace(data.URL)
}

func extractResultURLFromContent(raw any) string {
	switch content := raw.(type) {
	case string:
		url := strings.TrimSpace(content)
		if isLikelyURL(url) {
			return url
		}
	case map[string]any:
		if url := extractMediaURL(content["video_url"]); url != "" {
			return url
		}
		if url := extractMediaURL(content["url"]); url != "" {
			return url
		}
	default:
		var m map[string]any
		contentBytes, err := common.Marshal(content)
		if err == nil && common.Unmarshal(contentBytes, &m) == nil {
			if url := extractMediaURL(m["video_url"]); url != "" {
				return url
			}
			if url := extractMediaURL(m["url"]); url != "" {
				return url
			}
		}
	}
	return ""
}

func isLikelyURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

func failureReason(msg string, data queryData) string {
	if strings.TrimSpace(data.Error.Message) != "" {
		return strings.TrimSpace(data.Error.Message)
	}
	if strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	return "task failed"
}

func setMetadataIfNotZero(video *dto.OpenAIVideo, key string, val any) {
	switch v := val.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			video.SetMetadata(key, v)
		}
	case int:
		if v != 0 {
			video.SetMetadata(key, v)
		}
	}
}
