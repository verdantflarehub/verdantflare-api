package wxmaasseedance

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizedProviderTransportErrorDoesNotLeakProviderURLOrTaskID(t *testing.T) {
	err := sanitizedProviderTransportError("query", &url.Error{
		Op:  "Get",
		URL: "https://provider.example.com/v1/video/tasks/provider-secret-id",
		Err: errors.New("dial failed"),
	})
	require.EqualError(t, err, "provider query request failed")
	assert.NotContains(t, err.Error(), "provider-secret-id")
	assert.NotContains(t, err.Error(), "provider.example.com")
}

func TestValidateRequestRejectsMultiKeyChannelBeforeReadingRequest(t *testing.T) {
	t.Setenv("SD2_SUBMISSION_LEDGER_ENABLED", "true")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelIsMultiKey: true}}

	taskErr := (&TaskAdaptor{}).ValidateRequestAndSetAction(ctx, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_channel_config", taskErr.Code)
	assert.True(t, taskErr.LocalError)
	assert.NotContains(t, taskErr.Message, "wxmaas")
}

func TestValidateRequestRejectsWhenSubmissionLedgerIsDark(t *testing.T) {
	t.Setenv("SD2_SUBMISSION_LEDGER_ENABLED", "false")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	taskErr := (&TaskAdaptor{}).ValidateRequestAndSetAction(ctx, &relaycommon.RelayInfo{})
	require.NotNil(t, taskErr)
	assert.Equal(t, "submission_ledger_disabled", taskErr.Code)
}

func TestCreateRequestPreservesExplicitFalseSeedAndMediaOrder(t *testing.T) {
	duration := 15
	ratio := "9:16"
	resolution := "720p"
	generateAudio := false
	watermark := false
	seed := -1
	req := submitRequest{
		Model: PublicModel,
		Messages: []dto.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": contentTypeVideoURL, "video_url": map[string]any{"url": "https://assets.example.com/motion.mp4"}, "role": "reference_video"},
				map[string]any{"type": contentTypeText, "text": "  跟随参考素材生成视频  "},
				map[string]any{"type": contentTypeImageURL, "image_url": map[string]any{"url": "https://assets.example.com/character.png"}, "role": "reference_image"},
				map[string]any{"type": contentTypeText, "text": "保留原始内容顺序"},
				map[string]any{"type": contentTypeAudioURL, "audio_url": map[string]any{"url": "https://assets.example.com/music.mp3"}},
			},
		}},
		Duration:      &duration,
		Ratio:         &ratio,
		Resolution:    &resolution,
		GenerateAudio: &generateAudio,
		Watermark:     &watermark,
		Seed:          &seed,
	}

	taskReq, err := normalizeSubmitRequest(req)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: PublicModel}}
	providerReq, err := convertToCreateRequest(taskReq, info)
	require.NoError(t, err)
	require.Equal(t, UpstreamModel, info.UpstreamModelName)

	body, err := common.Marshal(providerReq)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"model":"doubao-seedance-2.0",
		"content":[
			{"type":"text","text":"跟随参考素材生成视频"},
			{"type":"video_url","video_url":{"url":"https://assets.example.com/motion.mp4"}},
			{"type":"image_url","image_url":{"url":"https://assets.example.com/character.png"}},
			{"type":"text","text":"保留原始内容顺序"},
			{"type":"audio_url","audio_url":{"url":"https://assets.example.com/music.mp3"}}
		],
		"resolution":"720p",
		"ratio":"9:16",
		"duration":15,
		"generate_audio":false,
		"watermark":false,
		"return_last_frame":false,
		"seed":-1
	}`, string(body))
	assert.NotContains(t, string(body), `"role"`)
	assert.NotContains(t, string(body), `"callback_url"`)
}

func TestNormalizeSubmitRequestDefaultsAndRejectsUnsafeInputs(t *testing.T) {
	duration := 4
	valid := submitRequest{
		Model:  PublicModel,
		Prompt: "生成一段视频",
	}
	taskReq, err := normalizeSubmitRequest(valid)
	require.NoError(t, err)
	providerReq, err := convertToCreateRequest(taskReq, &relaycommon.RelayInfo{})
	require.NoError(t, err)
	assert.Equal(t, defaultRatio, providerReq.Ratio)
	assert.Equal(t, defaultResolution, providerReq.Resolution)
	assert.Equal(t, defaultDuration, providerReq.Duration)
	assert.True(t, providerReq.GenerateAudio)
	assert.False(t, providerReq.Watermark)
	assert.False(t, providerReq.ReturnLastFrame)
	assert.Nil(t, providerReq.Seed)

	returnLastFrame := false
	badResolution := "1080p"
	tooShort := 3
	tests := []struct {
		name string
		req  submitRequest
		want string
	}{
		{
			name: "provider-controlled false remains forbidden",
			req:  submitRequest{Model: PublicModel, Prompt: "x", Duration: &duration, ReturnLastFrame: &returnLastFrame},
			want: "return_last_frame is not supported",
		},
		{
			name: "provider-controlled metadata remains forbidden",
			req:  submitRequest{Model: PublicModel, Prompt: "x", Duration: &duration, Metadata: map[string]any{"draft": false}},
			want: "draft is not supported",
		},
		{
			name: "resolution is fixed",
			req:  submitRequest{Model: PublicModel, Prompt: "x", Duration: &duration, Resolution: &badResolution},
			want: "resolution must be 720p",
		},
		{
			name: "duration below provider capability",
			req:  submitRequest{Model: PublicModel, Prompt: "x", Duration: &tooShort},
			want: "duration must be between 4 and 15 seconds",
		},
		{
			name: "messages and metadata content do not mix",
			req: submitRequest{
				Model:    PublicModel,
				Messages: []dto.Message{{Role: "user", Content: "x"}},
				Duration: &duration,
				Metadata: map[string]any{"content": []any{map[string]any{"type": "text", "text": "y"}}},
			},
			want: "messages content and metadata.content cannot be used together",
		},
		{
			name: "media must be HTTPS",
			req: submitRequest{
				Model: PublicModel,
				Messages: []dto.Message{{Role: "user", Content: []any{
					map[string]any{"type": "text", "text": "x"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://assets.example.com/a.png"}},
				}}},
				Duration: &duration,
			},
			want: "URL must use HTTPS",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeSubmitRequest(test.req)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestSubmitRequestRejectsUnknownAndProviderControlledTopLevelFields(t *testing.T) {
	var req submitRequest
	err := common.Unmarshal([]byte(`{"model":"verdantflare-sd2","prompt":"x","service_tier":"priority"}`), &req)
	require.EqualError(t, err, "unsupported request field: service_tier")

	err = common.Unmarshal([]byte(`{"model":"verdantflare-sd2","prompt":"x","return_last_frame":null}`), &req)
	require.EqualError(t, err, "return_last_frame is not supported")

	duration := 4
	_, err = normalizeSubmitRequest(submitRequest{
		Model: PublicModel, Prompt: "x", Duration: &duration,
		Metadata: map[string]any{"service_tier": "priority"},
	})
	require.EqualError(t, err, "service_tier is not supported")
}

func TestNormalizeSubmitRequestRejectsConflictingDurationAliases(t *testing.T) {
	duration := 10
	seconds := "5"
	_, err := normalizeSubmitRequest(submitRequest{
		Model: PublicModel, Prompt: "x", Duration: &duration, Seconds: &seconds,
	})
	require.EqualError(t, err, "duration and seconds must describe the same value")

	seconds = "10"
	taskReq, err := normalizeSubmitRequest(submitRequest{
		Model: PublicModel, Prompt: "x", Duration: &duration, Seconds: &seconds,
		Metadata: map[string]any{"duration": "10", "seconds": 10},
	})
	require.NoError(t, err)
	require.Equal(t, 10, taskReq.Duration)
}

func TestNormalizeSubmitRequestEnforcesMediaCountLimits(t *testing.T) {
	duration := 10
	content := []any{map[string]any{"type": "text", "text": "x"}}
	for index := 0; index < maxVideoCount+1; index++ {
		content = append(content, map[string]any{
			"type":      "video_url",
			"video_url": map[string]any{"url": "https://assets.example.com/" + string(rune('a'+index)) + ".mp4"},
		})
	}
	_, err := normalizeSubmitRequest(submitRequest{
		Model:    PublicModel,
		Messages: []dto.Message{{Role: "user", Content: content}},
		Duration: &duration,
	})
	require.EqualError(t, err, "at most 3 video references are supported")
}

func TestDoResponseAcceptsOnlyConfirmedTaskIDWithoutWritingClientResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_public"}}
	resp := &http.Response{
		StatusCode: http.StatusCreated,
		Body:       io.NopCloser(strings.NewReader(`{"task_id":"provider_task-123"}`)),
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(ctx, resp, info)
	require.Nil(t, taskErr)
	require.Equal(t, "provider_task-123", taskID)
	require.JSONEq(t, `{"schema_version":1,"provider_status":"queued","archive_state":"NONE"}`, string(taskData))
	require.Empty(t, recorder.Body.String(), "the controller must respond only after T3 commits")
}

func TestDoResponseTreatsEveryUnconfirmedOutcomeAsUnknown(t *testing.T) {
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_public"}}
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "bad request is not a definitive rejection", status: http.StatusBadRequest, body: `{"error":"invalid"}`},
		{name: "unauthorized is not a definitive rejection", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
		{name: "not found is not a definitive rejection", status: http.StatusNotFound, body: `{"error":"missing"}`},
		{name: "rate limited is not a definitive rejection", status: http.StatusTooManyRequests, body: `{"error":"rate limit"}`},
		{name: "server error is unknown", status: http.StatusBadGateway, body: `{"error":"bad gateway"}`},
		{name: "malformed success is unknown", status: http.StatusOK, body: `{`},
		{name: "empty task id is unknown", status: http.StatusOK, body: `{"task_id":""}`},
		{name: "unsafe task id is unknown", status: http.StatusOK, body: `{"task_id":"../provider"}`},
		{name: "oversized response is unknown", status: http.StatusOK, body: strings.Repeat("x", maxProviderResponseBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}
			taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(nil, resp, info)
			require.Empty(t, taskID)
			require.Empty(t, taskData)
			require.NotNil(t, taskErr)
			assert.Equal(t, "video_submission_unknown", taskErr.Code)
			assert.False(t, taskErr.LocalError)
			assert.Equal(t, map[string]any{"submission_state": "UNKNOWN", "task_id": "task_public"}, taskErr.Data)
		})
	}
}

func TestFetchTaskUsesGetAndDoesNotFollowRedirects(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, queryPathPrefix+"provider_task-123", r.URL.Path)
		require.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer provider.Close()

	resp, err := (&TaskAdaptor{}).FetchTask(provider.URL, "provider-key", map[string]any{"task_id": "provider_task-123"}, "")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.False(t, targetCalled)

	_, err = (&TaskAdaptor{}).FetchTask(provider.URL, "provider-key", map[string]any{"task_id": "../provider"}, "")
	require.ErrorContains(t, err, "invalid characters")

	_, err = (&TaskAdaptor{}).FetchTask(provider.URL, "provider-key", map[string]any{"task_id": 123}, "")
	require.ErrorContains(t, err, "must be a string")
}

func TestDoRequestDoesNotFollowRedirects(t *testing.T) {
	t.Setenv("SD2_SUBMISSION_LEDGER_ENABLED", "true")
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, createPath, r.URL.Path)
		require.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer provider.Close()

	adaptor := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelBaseUrl: provider.URL,
		ApiKey:         "provider-key",
	}}
	adaptor.Init(info)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	resp, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.False(t, targetCalled)
}

func TestParseTaskResultMapsDocumentedStatuses(t *testing.T) {
	tests := []struct {
		status       string
		expected     model.TaskStatus
		expectedWhy  string
		expectedProg string
	}{
		{status: "queued", expected: model.TaskStatusQueued, expectedProg: "20%"},
		{status: "running", expected: model.TaskStatusInProgress, expectedProg: "30%"},
		{status: "failed", expected: model.TaskStatusFailure, expectedWhy: "provider_failed", expectedProg: "100%"},
		{status: "expired", expected: model.TaskStatusUnknown, expectedWhy: "provider_expired", expectedProg: "100%"},
		{status: "cancelled", expected: model.TaskStatusUnknown, expectedWhy: "provider_cancelled", expectedProg: "100%"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			body := `{"id":"provider_task-123","status":"` + test.status + `","error":{"code":"raw-provider-code","message":"must not leak"}}`
			taskInfo, err := (&TaskAdaptor{}).ParseTaskResult([]byte(body))
			require.NoError(t, err)
			assert.Equal(t, string(test.expected), taskInfo.Status)
			assert.Equal(t, test.expectedWhy, taskInfo.Reason)
			assert.Equal(t, test.expectedProg, taskInfo.Progress)
			assert.NotContains(t, taskInfo.Reason, "raw-provider-code")
			assert.NotContains(t, taskInfo.Reason, "must not leak")
		})
	}

	succeeded, err := (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"id":"provider_task-123",
		"status":"succeeded",
		"content":{"video_url":"https://result.example.com/signed.mp4?token=secret"},
		"usage":{"completion_tokens":120,"total_tokens":130},
		"seed":-1,
		"resolution":"720p",
		"duration":10,
		"ratio":"16:9",
		"frames_per_second":24,
		"generate_audio":false,
		"created_at":101,
		"updated_at":202
	}`))
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusSuccess), succeeded.Status)
	assert.Equal(t, "https://result.example.com/signed.mp4?token=secret", succeeded.Url)
	assert.Equal(t, 120, succeeded.CompletionTokens)
	assert.Equal(t, 130, succeeded.TotalTokens)
	require.NotNil(t, succeeded.Seed)
	assert.Equal(t, int64(-1), *succeeded.Seed)
	assert.Equal(t, "720p", succeeded.Resolution)
	assert.Equal(t, 10, succeeded.Duration)
	assert.Equal(t, "16:9", succeeded.Ratio)
	assert.Equal(t, 24, succeeded.FramesPerSecond)
	require.NotNil(t, succeeded.GenerateAudio)
	assert.False(t, *succeeded.GenerateAudio)
	assert.Equal(t, int64(101), succeeded.ProviderCreatedAt)
	assert.Equal(t, int64(202), succeeded.ProviderUpdatedAt)
}

func TestParseTaskResultRejectsProtocolAmbiguity(t *testing.T) {
	tests := []string{
		`{"id":"provider_task-123","status":"succeeded","content":{}}`,
		`{"id":"provider_task-123","status":"mystery"}`,
		`{"id":"provider_task-123","status":"queued","usage":{"completion_tokens":1,"total_tokens":-1}}`,
		`{"id":"provider_task-123","status":"queued","usage":{"completion_tokens":1,"total_tokens":0}}`,
		`{"id":"provider_task-123","task_id":"provider_task-456","status":"queued"}`,
		`{"id":"provider_task-123","status":"queued","duration":16}`,
		`{"id":"provider_task-123","status":"queued","resolution":"1080p"}`,
		`{"id":"provider_task-123","status":"queued","frames_per_second":24,"framespersecond":30}`,
		`{"id":"provider_task-123","status":"queued","created_at":20,"updated_at":10}`,
		`{"status":"queued"}`,
		`{`,
		strings.Repeat("x", maxProviderResponseBytes+1),
	}
	for _, body := range tests {
		_, err := (&TaskAdaptor{}).ParseTaskResult([]byte(body))
		require.Error(t, err, body)
	}
}

func TestConvertToOpenAIVideoExposesOnlyVerdantFlareResultURL(t *testing.T) {
	generateAudio := false
	raw, err := common.Marshal(queryResponse{
		ID:              "provider_task-123",
		Status:          "succeeded",
		Content:         queryContent{VideoURL: "https://result.example.com/signed.mp4?token=secret"},
		Resolution:      "720p",
		Ratio:           "16:9",
		Duration:        15,
		FramesPerSecond: 30,
		GenerateAudio:   &generateAudio,
	})
	require.NoError(t, err)
	task := &model.Task{
		TaskID:     "task_public",
		Status:     model.TaskStatusSuccess,
		Progress:   "100%",
		CreatedAt:  100,
		FinishTime: 200,
		Properties: model.Properties{OriginModelName: PublicModel},
		Data:       raw,
	}

	body, err := (&TaskAdaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "result.example.com")
	assert.NotContains(t, string(body), "token=secret")

	var video dto.OpenAIVideo
	require.NoError(t, common.Unmarshal(body, &video))
	assert.Equal(t, dto.VideoStatusCompleted, video.Status)
	assert.Equal(t, PublicModel, video.Model)
	assert.Contains(t, video.Metadata["url"], "/v1/videos/task_public/content")
	assert.Equal(t, "720p", video.Metadata["resolution"])
	assert.Equal(t, "16:9", video.Metadata["ratio"])
	assert.Equal(t, float64(15), video.Metadata["duration"])
	assert.Equal(t, false, video.Metadata["generate_audio"])
}
