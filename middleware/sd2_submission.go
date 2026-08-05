package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	ContextKeySD2ClientRequestID = "sd2_client_request_id"
	ContextKeySD2RequestDigest   = "sd2_request_digest"
	ContextKeySD2DigestVersion   = "sd2_digest_version"
	ContextKeySD2RequestSummary  = "sd2_request_summary"
	ContextKeySD2Normalized      = "sd2_normalized_create"
)

// SD2SubmissionIdempotency performs the provider-independent T0 lookup before
// Distribute selects or validates a current channel. Existing submissions are
// replayed from their frozen state and can therefore recover even if routing or
// pricing configuration has changed since the original create.
func SD2SubmissionIdempotency() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !service.SD2SubmissionLedgerEnabled() {
			c.Next()
			return
		}
		if !isSD2CreateAlias(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}

		var request map[string]any
		if err := common.UnmarshalBodyReusable(c, &request); err != nil {
			// The task adaptor owns generic malformed-body responses. T0 only
			// handles requests that can be identified as verdantflare-sd2.
			c.Next()
			return
		}
		modelName, _ := request["model"].(string)
		if strings.TrimSpace(modelName) != service.SD2OriginModel {
			c.Next()
			return
		}

		clientRequestID, keyErr := ResolveSD2IdempotencyKey(
			c.GetHeader("Idempotency-Key"),
			c.GetHeader("X-Request-ID"),
		)
		if keyErr != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, &dto.TaskError{
				Code:       keyErr.Code,
				Message:    keyErr.Message,
				StatusCode: http.StatusBadRequest,
				LocalError: true,
			})
			return
		}

		normalized, err := service.NormalizeSD2CreateRequest(request, "generate")
		if err != nil {
			if errors.Is(err, service.ErrSD2SubmissionIdentityUnavailable) {
				writeSD2SubmissionError(c, http.StatusServiceUnavailable, "submission_identity_unavailable", "video submission identity is not configured", clientRequestID)
				return
			}
			code := "invalid_request"
			message := "invalid verdantflare-sd2 request"
			var requestErr *service.SD2CreateRequestError
			if errors.As(err, &requestErr) {
				code = requestErr.Code
				message = requestErr.Message
			}
			c.AbortWithStatusJSON(http.StatusBadRequest, &dto.TaskError{
				Code:       code,
				Message:    message,
				StatusCode: http.StatusBadRequest,
				LocalError: true,
			})
			return
		}

		existing, err := service.LookupTaskSubmission(c.GetInt("id"), clientRequestID, normalized.RequestDigest)
		if err != nil {
			if errors.Is(err, service.ErrTaskSubmissionIdempotencyConflict) {
				writeSD2SubmissionError(c, http.StatusConflict, "idempotency_conflict", "the idempotency key was already used for a different request", clientRequestID)
				return
			}
			writeSD2SubmissionError(c, http.StatusInternalServerError, "submission_lookup_failed", "unable to inspect the existing video submission", clientRequestID)
			return
		}
		if existing != nil {
			writeSD2SubmissionReplay(c, existing)
			return
		}
		if err := service.ValidateSD2CreateMediaAddresses(c.Request.Context(), request); err != nil {
			code := "invalid_media_url"
			message := "media references must resolve to public HTTPS addresses"
			var requestErr *service.SD2CreateRequestError
			if errors.As(err, &requestErr) {
				code = requestErr.Code
				message = requestErr.Message
			}
			writeSD2SubmissionError(c, http.StatusBadRequest, code, message, clientRequestID)
			return
		}

		c.Set(ContextKeySD2ClientRequestID, clientRequestID)
		c.Set(ContextKeySD2RequestDigest, normalized.RequestDigest)
		c.Set(ContextKeySD2DigestVersion, normalized.DigestVersion)
		c.Set(ContextKeySD2RequestSummary, normalized.RequestSummary)
		c.Set(ContextKeySD2Normalized, normalized)
		c.Next()
	}
}

type SD2IdempotencyKeyError struct {
	Code    string
	Message string
}

func (e *SD2IdempotencyKeyError) Error() string {
	return e.Message
}

// ResolveSD2IdempotencyKey accepts the standard header and the Skill's legacy
// alias, but never confuses either with the server trace header.
func ResolveSD2IdempotencyKey(idempotencyKey string, requestID string) (string, *SD2IdempotencyKeyError) {
	standard := strings.TrimSpace(idempotencyKey)
	alias := strings.TrimSpace(requestID)
	if standard == "" && alias == "" {
		return "", &SD2IdempotencyKeyError{Code: "missing_idempotency_key", Message: "Idempotency-Key or X-Request-ID is required"}
	}
	parse := func(value string) (string, error) {
		parsed, err := uuid.Parse(value)
		if err != nil {
			return "", err
		}
		return parsed.String(), nil
	}
	var normalizedStandard string
	var normalizedAlias string
	var err error
	if standard != "" {
		normalizedStandard, err = parse(standard)
		if err != nil {
			return "", &SD2IdempotencyKeyError{Code: "invalid_idempotency_key", Message: "Idempotency-Key must be a UUID"}
		}
	}
	if alias != "" {
		normalizedAlias, err = parse(alias)
		if err != nil {
			return "", &SD2IdempotencyKeyError{Code: "invalid_idempotency_key", Message: "X-Request-ID must be a UUID when used for video idempotency"}
		}
	}
	if normalizedStandard != "" && normalizedAlias != "" && normalizedStandard != normalizedAlias {
		return "", &SD2IdempotencyKeyError{Code: "conflicting_idempotency_keys", Message: "Idempotency-Key and X-Request-ID must identify the same request"}
	}
	if normalizedStandard != "" {
		return normalizedStandard, nil
	}
	return normalizedAlias, nil
}

func isSD2CreateAlias(method string, path string) bool {
	if method != http.MethodPost {
		return false
	}
	return path == "/v1/videos" || path == "/v1/video/generations"
}

func writeSD2SubmissionReplay(c *gin.Context, submission *model.TaskSubmission) {
	switch submission.State {
	case model.TaskSubmissionStateConfirmed:
		video := dto.NewOpenAIVideo()
		video.ID = submission.PublicTaskID
		video.TaskID = submission.PublicTaskID
		video.CreatedAt = submission.CreatedAt
		video.Model = submission.OriginModelName
		c.AbortWithStatusJSON(http.StatusOK, video)
	case model.TaskSubmissionStatePrepared, model.TaskSubmissionStateSending:
		writeSD2SubmissionError(c, http.StatusConflict, "submission_in_progress", "this video submission is already being processed", submission.ClientRequestID)
	case model.TaskSubmissionStateUnknown:
		writeSD2SubmissionError(c, http.StatusConflict, "submission_unknown", "the provider submission outcome is unknown and requires reconciliation", submission.ClientRequestID)
	case model.TaskSubmissionStateRejected:
		code := submission.SafeErrorCode
		if code == "" {
			code = "submission_rejected"
		}
		message := submission.SafeErrorMessage
		if message == "" {
			message = "the original video submission was rejected"
		}
		writeSD2SubmissionError(c, http.StatusConflict, code, message, submission.ClientRequestID)
	default:
		writeSD2SubmissionError(c, http.StatusConflict, "submission_in_progress", "this video submission cannot be submitted again", submission.ClientRequestID)
	}
}

func writeSD2SubmissionError(c *gin.Context, status int, code string, message string, clientRequestID string) {
	c.AbortWithStatusJSON(status, &dto.TaskError{
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
