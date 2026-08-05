package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

const videoProxyTimeout = 60 * time.Second

// videoProxyError returns a standardized OpenAI-style error response.
func videoProxyError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}

func VideoProxy(c *gin.Context) {
	taskID := c.Param("task_id")
	if taskID == "" {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "task_id is required")
		return
	}

	userID := c.GetInt("id")
	task, exists, err := model.GetByTaskId(userID, taskID)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to query task %s: %s", taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to query task")
		return
	}
	if !exists || task == nil {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return
	}

	if task.Status != model.TaskStatusSuccess {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("Task is not completed yet, current status: %s", task.Status))
		return
	}

	submission, submissionErr := model.GetTaskSubmissionByTaskDBID(task.ID)
	if submissionErr != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to query result ledger for task %s: %s", taskID, submissionErr.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to verify video result")
		return
	}
	snapshot, snapshotErr := service.DecodeSD2TaskResultSnapshot(task.Data)
	if submission != nil && snapshotErr != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Invalid archived result snapshot for task %s: %s", taskID, snapshotErr.Error()))
		videoProxyError(c, http.StatusConflict, "result_not_ready", "Video result is not safely archived")
		return
	}
	if submission == nil && snapshotErr == nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Archived result snapshot has no ledger for task %s", taskID))
		videoProxyError(c, http.StatusConflict, "result_not_ready", "Video result is not safely archived")
		return
	}
	if submission != nil {
		frozenDescriptor, descriptorErr := service.SD2ArchivedResultFromSubmission(submission)
		if submission.UserID != userID || submission.PublicTaskID != task.TaskID ||
			submission.BillingState != model.TaskSubmissionBillingStateSettled ||
			submission.PollState != model.TaskSubmissionPollStateTerminal || descriptorErr != nil ||
			snapshot.ArchiveState != service.SD2ArchiveStateArchived || snapshot.ArchivedResult == nil ||
			*snapshot.ArchivedResult != frozenDescriptor {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Archived result ledger mismatch for task %s", taskID))
			videoProxyError(c, http.StatusConflict, "result_not_ready", "Video result is not safely archived")
			return
		}
		archivedObject, openErr := service.OpenSD2ArchivedResult(c.Request.Context(), frozenDescriptor)
		if openErr != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to open archived result for task %s", taskID))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to open archived video result")
			return
		}
		defer archivedObject.Body.Close()
		if streamErr := writeVerifiedArchivedVideoResponse(c, archivedObject, frozenDescriptor); streamErr != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to stream archived result for task %s", taskID))
			if !c.Writer.Written() {
				videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to stream archived video result")
			}
		}
		return
	}

	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to get channel for task %s: %s", taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to retrieve channel information")
		return
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	var videoURL string
	proxy := channel.GetSetting().Proxy
	client := service.GetSSRFProtectedHTTPClient()
	if proxy != "" {
		// 渠道代理路径的连接由代理侧建立，无法做拨号时逐 IP 校验，
		// 因此后面对 videoURL 保留请求前的一次性 SSRF 校验。
		client, err = service.GetHttpClientWithProxy(proxy)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to create proxy client for task %s: %s", taskID, err.Error()))
			videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy client")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), videoProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to create request: %s", err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy request")
		return
	}
	req.Header.Set("Accept", "video/*, application/octet-stream")
	req.Header.Set("Accept-Encoding", "identity")

	switch channel.Type {
	case constant.ChannelTypeGemini:
		apiKey := task.PrivateData.Key
		if apiKey == "" {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Missing stored API key for Gemini task %s", taskID))
			videoProxyError(c, http.StatusInternalServerError, "server_error", "API key not stored for task")
			return
		}
		videoURL, err = getGeminiVideoURL(channel, task, apiKey)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to resolve Gemini video URL for task %s", taskID))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to resolve Gemini video URL")
			return
		}
		req.Header.Set("x-goog-api-key", apiKey)
	case constant.ChannelTypeVertexAi:
		videoURL, err = getVertexVideoURL(channel, task)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to resolve Vertex video URL for task %s", taskID))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to resolve Vertex video URL")
			return
		}
	case constant.ChannelTypeOpenAI, constant.ChannelTypeSora:
		videoURL = fmt.Sprintf("%s/v1/videos/%s/content", baseURL, task.GetUpstreamTaskID())
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	default:
		// Video URL is stored in PrivateData.ResultURL (fallback to FailReason for old data)
		videoURL = task.GetResultURL()
	}

	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Video URL is empty for task %s", taskID))
		videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
		return
	}

	if strings.HasPrefix(videoURL, "data:") {
		if err := writeVideoDataURL(c, videoURL); err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to decode video data URL for task %s: %s", taskID, err.Error()))
			if !c.Writer.Written() {
				videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
			}
		}
		return
	}

	var validateErr error
	if proxy == "" {
		validateErr = service.ValidateSSRFProtectedFetchURL(videoURL)
	} else {
		fetchSetting := system_setting.GetFetchSetting()
		validateErr = common.ValidateURLWithFetchSetting(videoURL, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, fetchSetting.ApplyIPFilterForDomain)
	}
	if validateErr != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Video URL blocked by fetch policy for task %s", taskID))
		videoProxyError(c, http.StatusForbidden, "server_error", "request blocked by video fetch policy")
		return
	}

	req.URL, err = url.Parse(videoURL)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to parse video URL for task %s", taskID))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy request")
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to fetch video for task %s from host %s", taskID, req.URL.Hostname()))
		videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Upstream returned status %d for task %s from host %s", resp.StatusCode, taskID, req.URL.Hostname()))
		videoProxyError(c, http.StatusBadGateway, "server_error",
			fmt.Sprintf("Upstream service returned status %d", resp.StatusCode))
		return
	}

	if err = writePrivateVideoResponse(c, resp.Body, resp.Header.Get("Content-Type"), resp.ContentLength); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to stream video content: %s", err.Error()))
		if !c.Writer.Written() {
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to stream video content")
		}
	}
}

// writeVerifiedArchivedVideoResponse consumes and verifies the complete
// private object before writing response headers. This keeps a late size or
// checksum failure from being disguised as a successful HTTP 200 download.
func writeVerifiedArchivedVideoResponse(c *gin.Context, object *service.SD2ArchivedObject, expected service.SD2ArchivedResult) error {
	if object == nil || object.Body == nil || object.Size != expected.Size ||
		object.ContentType != expected.ContentType || object.SHA256 != expected.SHA256 {
		return service.ErrInvalidSD2ArchivedResult
	}
	tempFile, err := os.CreateTemp("", "verdantflare-sd2-proxy-*.video")
	if err != nil {
		return fmt.Errorf("create verified video response buffer")
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}()
	if err := tempFile.Chmod(0o600); err != nil {
		return fmt.Errorf("secure verified video response buffer")
	}

	digest := sha256.New()
	limited := &io.LimitedReader{R: object.Body, N: expected.Size + 1}
	written, err := io.Copy(io.MultiWriter(tempFile, digest), limited)
	if err != nil {
		return fmt.Errorf("verify archived video body: %w", err)
	}
	if written != expected.Size || limited.N != 1 || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return service.ErrInvalidSD2ArchivedResult
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("prepare verified video response")
	}
	return writePrivateVideoResponse(c, tempFile, expected.ContentType, expected.Size)
}

func writeVideoDataURL(c *gin.Context, dataURL string) error {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid data url")
	}

	header := parts[0]
	payload := parts[1]
	if !strings.HasPrefix(header, "data:") || !strings.Contains(header, ";base64") {
		return fmt.Errorf("unsupported data url")
	}

	mimeType := strings.TrimPrefix(header, "data:")
	mimeType = strings.TrimSuffix(mimeType, ";base64")
	if mimeType == "" {
		mimeType = "video/mp4"
	}
	if !isAllowedVideoProxyContentType(mimeType) {
		return fmt.Errorf("unsupported video data url content type")
	}
	maxEncodedLength := (service.MaxSD2ArchivedVideoBytes + 2) / 3 * 4
	if int64(len(payload)) > maxEncodedLength {
		return fmt.Errorf("video data url exceeds size limit")
	}
	if int64(base64.StdEncoding.DecodedLen(len(payload))) > service.MaxSD2ArchivedVideoBytes {
		return fmt.Errorf("video data url exceeds size limit")
	}

	videoBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		videoBytes, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return err
		}
	}
	if int64(len(videoBytes)) > service.MaxSD2ArchivedVideoBytes {
		return fmt.Errorf("video data url exceeds size limit")
	}

	return writePrivateVideoResponse(c, bytes.NewReader(videoBytes), mimeType, int64(len(videoBytes)))
}

func writePrivateVideoResponse(c *gin.Context, body io.Reader, contentType string, contentLength int64) error {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !isAllowedVideoProxyContentType(mediaType) {
		return fmt.Errorf("unsupported video content type")
	}
	if contentLength > service.MaxSD2ArchivedVideoBytes {
		return fmt.Errorf("video content exceeds size limit")
	}

	c.Writer.Header().Set("Content-Type", mediaType)
	c.Writer.Header().Set("Content-Disposition", "attachment; filename=video")
	c.Writer.Header().Set("Cache-Control", "private, no-store")
	c.Writer.Header().Set("Pragma", "no-cache")
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	if contentLength >= 0 {
		c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", contentLength))
	}
	c.Writer.WriteHeader(http.StatusOK)

	if contentLength >= 0 {
		_, err = io.CopyN(c.Writer, body, contentLength)
		return err
	}
	_, err = io.Copy(c.Writer, io.LimitReader(body, service.MaxSD2ArchivedVideoBytes))
	if err != nil {
		return err
	}
	var extra [1]byte
	if n, readErr := body.Read(extra[:]); n > 0 || (readErr != nil && readErr != io.EOF) {
		return fmt.Errorf("video content exceeds size limit")
	}
	return nil
}

func isAllowedVideoProxyContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	return strings.HasPrefix(mediaType, "video/") || mediaType == "application/octet-stream"
}
