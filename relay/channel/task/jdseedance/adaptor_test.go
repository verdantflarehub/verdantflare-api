package jdseedance

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSubmitRequestFromMessages(t *testing.T) {
	generateAudio := true
	watermark := false
	req := submitRequest{
		Model: ModelJDSeedanceSD,
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":      contentTypeVideoURL,
						"video_url": map[string]any{"url": "https://example.com/input.mp4"},
					},
					map[string]any{
						"type":      contentTypeAudioURL,
						"audio_url": map[string]any{"url": "https://example.com/input.mp3"},
					},
					map[string]any{
						"type": contentTypeText,
						"text": "请将视频和音频合成为舞蹈",
					},
				},
			},
		},
		Metadata: map[string]any{
			"ratio": "9:16",
		},
		Duration:      10,
		GenerateAudio: &generateAudio,
		Watermark:     &watermark,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	require.Equal(t, ModelJDSeedanceSD, taskReq.Model)
	require.Equal(t, "请将视频和音频合成为舞蹈", taskReq.Prompt)

	createReq, err := convertToCreateRequest(taskReq)
	require.NoError(t, err)
	require.Equal(t, "9:16", createReq.Ratio)
	require.Equal(t, 10, createReq.Duration)
	require.True(t, createReq.GenerateAudio)
	require.False(t, createReq.Watermark)
	require.Len(t, createReq.Content, 3)
	require.Equal(t, contentTypeVideoURL, createReq.Content[0].Type)
	require.Equal(t, "https://example.com/input.mp4", createReq.Content[0].VideoURL.URL)
	require.Equal(t, contentTypeAudioURL, createReq.Content[1].Type)
	require.Equal(t, "https://example.com/input.mp3", createReq.Content[1].AudioURL.URL)
	require.Equal(t, contentTypeText, createReq.Content[2].Type)
}

func TestNormalizeSubmitRequestFromPromptAndMetadataContent(t *testing.T) {
	req := submitRequest{
		Model:  ModelJDSeedanceSD,
		Prompt: "第一人称视角果茶宣传广告",
		Metadata: map[string]any{
			"content": []any{
				map[string]any{
					"type": contentTypeText,
					"text": "第一人称视角果茶宣传广告",
				},
			},
			"duration": "11",
		},
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	createReq, err := convertToCreateRequest(taskReq)
	require.NoError(t, err)
	require.Equal(t, defaultRatio, createReq.Ratio)
	require.Equal(t, 11, createReq.Duration)
	require.True(t, createReq.GenerateAudio)
	require.False(t, createReq.Watermark)
	require.Len(t, createReq.Content, 1)
	require.Equal(t, "第一人称视角果茶宣传广告", createReq.Content[0].Text)
}

func TestDoResponseReturnsPublicTaskIDAndStoresUpstreamID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{
		OriginModelName: ModelJDSeedanceSD,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID: "task_local",
		},
	}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{"code":1,"data":"jd_task_123","msg":"success"}`)),
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Nil(t, taskErr)
	require.Equal(t, "jd_task_123", taskID)
	require.JSONEq(t, `{"code":1,"data":"jd_task_123","msg":"success"}`, string(taskData))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"id":"task_local"`)
}

func TestDoResponseAcceptsZeroCodeSuccessWithObjectTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{
		OriginModelName: ModelJDSeedanceSD,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID: "task_local",
		},
	}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"taskId":"jd_task_456"},"msg":"成功"}`)),
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Nil(t, taskErr)
	require.Equal(t, "jd_task_456", taskID)
	require.JSONEq(t, `{"code":0,"data":{"taskId":"jd_task_456"},"msg":"成功"}`, string(taskData))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"id":"task_local"`)
}

func TestDoResponseAcceptsNestedSnakeTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_local"}}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"result":{"task_id":"jd_task_789"}},"msg":"success"}`)),
	}

	taskID, _, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Nil(t, taskErr)
	require.Equal(t, "jd_task_789", taskID)
}

func TestDoResponseRejectsFailedCreateEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_local"}}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":"","msg":"quota exceeded"}`)),
	}

	taskID, _, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Empty(t, taskID)
	require.NotNil(t, taskErr)
	require.Equal(t, "video_generation_create_failed", taskErr.Code)
}

func TestDoResponseRejectsSuccessfulEnvelopeWithoutTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_local"}}
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{},"msg":"成功"}`)),
	}

	taskID, _, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Empty(t, taskID)
	require.NotNil(t, taskErr)
	require.Equal(t, "invalid_response", taskErr.Code)
}

func TestWhiteLabelUpstreamMessageRemovesProviderNames(t *testing.T) {
	require.Equal(t, "video generation quota exceeded", whiteLabelUpstreamMessage("JD Seedance quota exceeded"))
	require.Equal(t, "upstream request failed", whiteLabelUpstreamMessage("京东 request failed"))
}

func TestFetchTaskPostsDanceQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, queryPath, r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer jd-key", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"taskId":"jd_task_123"}`, string(body))
		_, _ = w.Write([]byte(`{"code":1,"data":{"id":"jd_task_123","status":"processing"},"msg":"success"}`))
	}))
	defer server.Close()

	resp, err := (&TaskAdaptor{}).FetchTask(server.URL, "jd-key", map[string]any{"task_id": "jd_task_123"}, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestParseTaskResultStatusAndURL(t *testing.T) {
	taskInfo, err := (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"code": 1,
		"data": {
			"id": "jd_task_123",
			"model": "dance-2.0",
			"status": "success",
			"content": "https://example.com/result.mp4",
			"ratio": "16:9",
			"duration": 11,
			"usage": {"completion_tokens": 108900, "total_tokens": 108900}
		},
		"msg": "success"
	}`))
	require.NoError(t, err)
	require.Equal(t, "SUCCESS", taskInfo.Status)
	require.Equal(t, "100%", taskInfo.Progress)
	require.Equal(t, "https://example.com/result.mp4", taskInfo.Url)
	require.Equal(t, 108900, taskInfo.CompletionTokens)
	require.Equal(t, 108900, taskInfo.TotalTokens)

	taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"code": 0,
		"data": {
			"taskId": "jd_task_456",
			"state": "running"
		},
		"msg": "成功"
	}`))
	require.NoError(t, err)
	require.Equal(t, "jd_task_456", taskInfo.TaskID)
	require.Equal(t, "IN_PROGRESS", taskInfo.Status)
	require.Equal(t, "50%", taskInfo.Progress)

	taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"code": 1,
		"data": {
			"id": "jd_task_123",
			"status": "success",
			"content": {"url": "https://example.com/content-result.mp4"},
			"video_url": "https://example.com/top-level-result.mp4"
		},
		"msg": "success"
	}`))
	require.NoError(t, err)
	require.Equal(t, "https://example.com/content-result.mp4", taskInfo.Url)

	taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"code": 1,
		"data": {
			"id": "jd_task_123",
			"status": "success",
			"content": "not a url",
			"url": "https://example.com/fallback-result.mp4"
		},
		"msg": "success"
	}`))
	require.NoError(t, err)
	require.Equal(t, "https://example.com/fallback-result.mp4", taskInfo.Url)

	taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(`{"code":1,"data":{"id":"jd_task_123","status":"processing"},"msg":"success"}`))
	require.NoError(t, err)
	require.Equal(t, "IN_PROGRESS", taskInfo.Status)
	require.Equal(t, "50%", taskInfo.Progress)

	taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(`{"code":1,"data":{"id":"jd_task_123","status":"failed","error":{"code":"E1","message":"bad input"}},"msg":"success"}`))
	require.NoError(t, err)
	require.Equal(t, "FAILURE", taskInfo.Status)
	require.Equal(t, "bad input", taskInfo.Reason)
}

func TestConvertToOpenAIVideoIncludesResultMetadata(t *testing.T) {
	raw := []byte(`{
		"code": 1,
		"data": {
			"id": "jd_task_123",
			"model": "dance-2.0",
			"status": "success",
			"content": {"video_url": "https://example.com/result.mp4?X-Tos-Algorithm=TOS4-HMAC-SHA256&X-Tos-Signature=abc"},
			"seed": 97257,
			"execution_expires_after": 172800,
			"usage": {"completion_tokens": 108900, "total_tokens": 108900},
			"priority": 0,
			"ratio": "16:9",
			"duration": 11,
			"resolution": "1920x1080",
			"framespersecond": 30,
			"generate_audio": true,
			"draft": false,
			"service_tier": "default",
			"updated_at": 121
		},
		"msg": "success"
	}`)
	task := &model.Task{
		TaskID:     "task_local",
		Status:     model.TaskStatusSuccess,
		Progress:   "100%",
		CreatedAt:  100,
		FinishTime: 120,
		Properties: model.Properties{OriginModelName: ModelJDSeedanceSD},
		Data:       raw,
	}

	body, err := (&TaskAdaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)

	var video dto.OpenAIVideo
	require.NoError(t, common.Unmarshal(body, &video))
	require.Equal(t, "task_local", video.ID)
	require.Equal(t, dto.VideoStatusCompleted, video.Status)
	require.Equal(t, 100, video.Progress)
	require.Equal(t, "https://example.com/result.mp4?X-Tos-Algorithm=TOS4-HMAC-SHA256&X-Tos-Signature=abc", video.Metadata["url"])
	require.Contains(t, string(body), "&X-Tos-Signature=abc")
	require.NotContains(t, string(body), `\u0026`)
	require.Equal(t, "16:9", video.Metadata["ratio"])
	require.Equal(t, float64(11), video.Metadata["duration"])
	require.Equal(t, "1920x1080", video.Metadata["resolution"])
	require.Equal(t, float64(30), video.Metadata["framespersecond"])
	require.Equal(t, true, video.Metadata["generate_audio"])
	require.Equal(t, float64(97257), video.Metadata["seed"])
	require.Equal(t, float64(172800), video.Metadata["execution_expires_after"])
	require.Equal(t, float64(0), video.Metadata["priority"])
	require.Equal(t, false, video.Metadata["draft"])
	require.Equal(t, "default", video.Metadata["service_tier"])
	require.Equal(t, float64(121), video.Metadata["updated_at"])
	require.Equal(t, map[string]any{"completion_tokens": float64(108900), "total_tokens": float64(108900)}, video.Metadata["usage"])
}
