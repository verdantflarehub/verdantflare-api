package jdseedance

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	require.Equal(t, defaultVideoRole, createReq.Content[0].Role)
	require.Equal(t, contentTypeAudioURL, createReq.Content[1].Type)
	require.Equal(t, "https://example.com/input.mp3", createReq.Content[1].AudioURL.URL)
	require.Equal(t, contentTypeText, createReq.Content[2].Type)
}

func TestNormalizeSubmitRequestKeepsPromptFirstAndLegacyAudio(t *testing.T) {
	req := submitRequest{
		Model:    ModelJDSeedanceSD,
		Prompt:   "先看提示词",
		Image:    "https://example.com/image.png",
		Audio:    "https://example.com/audio.mp3",
		Duration: 10,
	}
	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	createReq, err := convertToCreateRequest(taskReq)
	require.NoError(t, err)
	require.Len(t, createReq.Content, 3)
	require.Equal(t, contentTypeText, createReq.Content[0].Type)
	require.Equal(t, contentTypeImageURL, createReq.Content[1].Type)
	require.Equal(t, contentTypeAudioURL, createReq.Content[2].Type)
}

func TestNormalizeSubmitRequestRejectsConflictingDurationAliases(t *testing.T) {
	_, err := normalizeSubmitRequest(submitRequest{
		Model:    ModelJDSeedanceSD,
		Prompt:   "x",
		Duration: 10,
		Seconds:  "5",
	})
	require.EqualError(t, err, "duration and seconds must describe the same value")

	taskReq, err := normalizeSubmitRequest(submitRequest{
		Model:    ModelJDSeedanceSD,
		Prompt:   "x",
		Duration: 10,
		Seconds:  "10",
		Metadata: map[string]any{"duration": "10", "seconds": 10},
	})
	require.NoError(t, err)
	require.Equal(t, 10, taskReq.Duration)
	require.Equal(t, 10, taskReq.Metadata["duration"])
	require.NotContains(t, taskReq.Metadata, "seconds")
}

func TestNormalizeSubmitRequestPreservesThreeVideoReferences(t *testing.T) {
	req := submitRequest{
		Model: ModelJDSeedanceSD,
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": contentTypeText, "text": "依次参考视频1、视频2和视频3"},
					map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://example.com/1.mp4"}},
					map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://example.com/2.mp4"}},
					map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://example.com/3.mp4"}},
				},
			},
		},
		Duration: 10,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	createReq, err := convertToCreateRequest(taskReq)
	require.NoError(t, err)
	require.Len(t, createReq.Content, 4)
	require.Equal(t, contentTypeText, createReq.Content[0].Type)
	for index := 1; index <= maxVideoReferences; index++ {
		require.Equal(t, contentTypeVideoURL, createReq.Content[index].Type)
		require.Equal(t, "https://example.com/"+string(rune('0'+index))+".mp4", createReq.Content[index].VideoURL.URL)
		require.Equal(t, defaultVideoRole, createReq.Content[index].Role)
	}
}

func TestNormalizeSubmitRequestAddsAndPreservesMediaRoles(t *testing.T) {
	req := submitRequest{
		Model: ModelJDSeedanceSD,
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": contentTypeText, "text": "保持图片1和图片2中的人物与环境"},
					map[string]any{"type": contentTypeImageURL, "image_url": map[string]any{"url": "https://example.com/character.png"}},
					map[string]any{"type": contentTypeImageURL, "image_url": map[string]any{"url": "https://example.com/environment.png"}, "role": "first_frame"},
					map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://example.com/control.mp4"}},
				},
			},
		},
		Duration: 15,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	createReq, err := convertToCreateRequest(taskReq)
	require.NoError(t, err)
	require.Len(t, createReq.Content, 4)
	require.Equal(t, defaultImageRole, createReq.Content[1].Role)
	require.Equal(t, "first_frame", createReq.Content[2].Role)
	require.Equal(t, defaultVideoRole, createReq.Content[3].Role)

	body, err := common.Marshal(createReq)
	require.NoError(t, err)
	require.Contains(t, string(body), `"role":"reference_image"`)
	require.Contains(t, string(body), `"role":"first_frame"`)
	require.Contains(t, string(body), `"role":"reference_video"`)
}

func TestNormalizeSubmitRequestAddsMediaRolesForLegacyInputs(t *testing.T) {
	tests := []struct {
		name               string
		req                submitRequest
		expectedVideoCount int
	}{
		{
			name: "top-level image and video fields",
			req: submitRequest{
				Model:    ModelJDSeedanceSD,
				Prompt:   "保持两张参考图片中的角色与环境，并遵循参考视频运动",
				Image:    "https://example.com/character.png",
				Images:   []string{"https://example.com/environment.png"},
				Video:    "https://example.com/control-1.mp4",
				Videos:   []string{"https://example.com/control-2.mp4"},
				Duration: 15,
			},
			expectedVideoCount: 2,
		},
		{
			name: "metadata image and video content",
			req: submitRequest{
				Model:  ModelJDSeedanceSD,
				Prompt: "保持参考图片并遵循参考视频运动",
				Metadata: map[string]any{
					"content": []any{
						map[string]any{"type": contentTypeText, "text": "保持参考图片并遵循参考视频运动"},
						map[string]any{"type": contentTypeImageURL, "image_url": map[string]any{"url": "https://example.com/reference.png"}},
						map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://example.com/reference-typed.mp4"}},
						map[string]any{"video_url": map[string]any{"url": "https://example.com/reference-fallback.mp4"}},
					},
					"duration": 15,
				},
			},
			expectedVideoCount: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			taskReq, err := normalizeSubmitRequest(test.req)
			require.NoError(t, err)
			createReq, err := convertToCreateRequest(taskReq)
			require.NoError(t, err)

			imageCount := 0
			videoCount := 0
			for _, item := range createReq.Content {
				switch item.Type {
				case contentTypeImageURL:
					imageCount++
					require.Equal(t, defaultImageRole, item.Role)
				case contentTypeVideoURL:
					videoCount++
					require.Equal(t, defaultVideoRole, item.Role)
				}
			}
			require.NotZero(t, imageCount)
			require.Equal(t, test.expectedVideoCount, videoCount)
		})
	}
}

func TestNormalizeSubmitRequestRejectsUnsupportedVideoRole(t *testing.T) {
	req := submitRequest{
		Model: ModelJDSeedanceSD,
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": contentTypeText, "text": "参考视频生成"},
					map[string]any{
						"type":      contentTypeVideoURL,
						"video_url": map[string]any{"url": "https://example.com/reference.mp4"},
						"role":      "first_frame",
					},
				},
			},
		},
		Duration: 10,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	_, err = convertToCreateRequest(taskReq)
	require.EqualError(t, err, "video_url.role must be reference_video")
}

func TestNormalizeSubmitRequestRejectsFourthVideoReference(t *testing.T) {
	content := []any{
		map[string]any{"type": contentTypeText, "text": "参考视频生成"},
	}
	for index := 1; index <= maxVideoReferences+1; index++ {
		content = append(content, map[string]any{
			"type":      contentTypeVideoURL,
			"video_url": map[string]any{"url": "https://example.com/reference.mp4"},
		})
	}
	req := submitRequest{
		Model:    ModelJDSeedanceSD,
		Messages: []dto.Message{{Role: "user", Content: content}},
		Duration: 10,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	_, err = convertToCreateRequest(taskReq)
	require.EqualError(t, err, "at most 3 video references are supported")
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

func TestDoResponseReturnsParsedResultWithoutWritingClientResponse(t *testing.T) {
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
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
}

func TestDoResponseBoundsAndRedactsInvalidProviderBody(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxCreateResponseBytes+1)))}
	_, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, &relaycommon.RelayInfo{})
	require.NotNil(t, taskErr)
	require.Nil(t, taskData)
	require.NotContains(t, taskErr.Message, strings.Repeat("x", 32))

	resp = &http.Response{Body: io.NopCloser(strings.NewReader(`{"secret_url":"https://example.com/video.mp4?token=secret"`))}
	_, taskData, taskErr = (&TaskAdaptor{}).DoResponse(c, resp, &relaycommon.RelayInfo{})
	require.NotNil(t, taskErr)
	require.Nil(t, taskData)
	require.NotContains(t, taskErr.Message, "secret")
}

func TestParseTaskResultRejectsUnknownStatusAndNon720P(t *testing.T) {
	adaptor := &TaskAdaptor{}
	_, err := adaptor.ParseTaskResult([]byte(`{"code":0,"data":{"id":"provider-task","status":"mystery","resolution":"720P"}}`))
	require.ErrorContains(t, err, "unknown provider status")

	_, err = adaptor.ParseTaskResult([]byte(`{"code":0,"data":{"id":"provider-task","status":"running","resolution":"1080P"}}`))
	require.ErrorContains(t, err, "unexpected resolution")
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
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
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

func TestDoResponseRejectsImageSafetyError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_local"}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"ErrorCode":"InputImageSensitiveContentDetected.PrivacyInformation",
			"ErrorMessage":"The request failed because the input image may contain real person. Request id: upstream_request_123"
		}`)),
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Empty(t, taskID)
	require.NotEmpty(t, taskData)
	require.NotNil(t, taskErr)
	require.Equal(t, "input_image_safety_check_failed", taskErr.Code)
	require.Equal(t, "The reference image did not pass the content safety check", taskErr.Message)
	require.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
	require.True(t, taskErr.LocalError)
	require.NotContains(t, taskErr.Message, "upstream_request_123")
}

func TestDoResponseRejectsOtherTopLevelUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_local"}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"ErrorCode":"InvalidParameter",
			"ErrorMessage":"The video generation request is invalid"
		}`)),
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)
	require.Empty(t, taskID)
	require.NotEmpty(t, taskData)
	require.NotNil(t, taskErr)
	require.Equal(t, "video_generation_create_failed", taskErr.Code)
	require.Equal(t, "The video generation request is invalid", taskErr.Message)
	require.Equal(t, http.StatusBadGateway, taskErr.StatusCode)
	require.False(t, taskErr.LocalError)
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

func TestCreateAndQueryNeverFollowProviderRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var destinationHits atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				destinationHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer destination.Close()

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
			}))
			defer source.Close()

			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", nil)
			adaptor := &TaskAdaptor{apiKey: "jd-key", baseURL: source.URL}
			createResp, err := adaptor.DoRequest(c, &relaycommon.RelayInfo{}, strings.NewReader(`{"content":[]}`))
			require.NoError(t, err)
			require.Equal(t, status, createResp.StatusCode)
			require.NoError(t, createResp.Body.Close())

			queryResp, err := adaptor.FetchTask(source.URL, "jd-key", map[string]any{"task_id": "jd_task_123"}, "")
			require.NoError(t, err)
			require.Equal(t, status, queryResp.StatusCode)
			require.NoError(t, queryResp.Body.Close())
			require.Zero(t, destinationHits.Load())
		})
	}
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
			"resolution": "720p",
			"framespersecond": 30,
			"seed": 42,
			"generate_audio": false,
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
	require.Equal(t, "720p", taskInfo.Resolution)
	require.Equal(t, 11, taskInfo.Duration)
	require.Equal(t, "16:9", taskInfo.Ratio)
	require.Equal(t, 30, taskInfo.FramesPerSecond)
	require.NotNil(t, taskInfo.Seed)
	require.Equal(t, int64(42), *taskInfo.Seed)
	require.NotNil(t, taskInfo.GenerateAudio)
	require.False(t, *taskInfo.GenerateAudio)

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

	for status, reason := range map[string]string{
		"cancelled": "provider_cancelled",
		"expired":   "provider_expired",
	} {
		taskInfo, err = (&TaskAdaptor{}).ParseTaskResult([]byte(fmt.Sprintf(`{"code":1,"data":{"id":"jd_task_123","status":%q},"msg":"success"}`, status)))
		require.NoError(t, err)
		require.Equal(t, "UNKNOWN", taskInfo.Status)
		require.Equal(t, reason, taskInfo.Reason)
	}
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
