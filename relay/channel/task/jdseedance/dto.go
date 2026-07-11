package jdseedance

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/dto"
)

type mediaURL struct {
	URL string `json:"url,omitempty"`
}

type contentItem struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *mediaURL `json:"image_url,omitempty"`
	VideoURL *mediaURL `json:"video_url,omitempty"`
	AudioURL *mediaURL `json:"audio_url,omitempty"`
}

type submitRequest struct {
	Model         string         `json:"model"`
	Prompt        string         `json:"prompt,omitempty"`
	Messages      []dto.Message  `json:"messages,omitempty"`
	Image         string         `json:"image,omitempty"`
	Images        []string       `json:"images,omitempty"`
	Duration      int            `json:"duration,omitempty"`
	Seconds       string         `json:"seconds,omitempty"`
	Ratio         string         `json:"ratio,omitempty"`
	GenerateAudio *bool          `json:"generate_audio,omitempty"`
	Watermark     *bool          `json:"watermark,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type createRequest struct {
	Content       []contentItem `json:"content"`
	GenerateAudio bool          `json:"generate_audio"`
	Ratio         string        `json:"ratio"`
	Duration      int           `json:"duration"`
	Watermark     bool          `json:"watermark"`
}

type createResponse struct {
	Code int    `json:"code"`
	Data any    `json:"data"`
	Msg  string `json:"msg"`
}

type queryRequest struct {
	TaskID string `json:"taskId"`
}

type queryResponse struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
	Msg  string          `json:"msg"`
}

type queryData struct {
	ID                    string  `json:"id"`
	TaskID                string  `json:"taskId"`
	TaskIDSnake           string  `json:"task_id"`
	Model                 string  `json:"model"`
	Status                string  `json:"status"`
	State                 string  `json:"state"`
	Content               any     `json:"content"`
	VideoURL              string  `json:"video_url,omitempty"`
	URL                   string  `json:"url,omitempty"`
	CreatedAt             int64   `json:"created_at"`
	UpdatedAt             int64   `json:"updated_at"`
	Seed                  *int    `json:"seed,omitempty"`
	Resolution            string  `json:"resolution,omitempty"`
	Ratio                 string  `json:"ratio,omitempty"`
	Duration              int     `json:"duration,omitempty"`
	FramesPerSecond       int     `json:"framespersecond,omitempty"`
	ServiceTier           string  `json:"service_tier,omitempty"`
	ExecutionExpiresAfter *int    `json:"execution_expires_after,omitempty"`
	GenerateAudio         bool    `json:"generate_audio,omitempty"`
	Priority              *int    `json:"priority,omitempty"`
	Draft                 *bool   `json:"draft,omitempty"`
	Usage                 *usage  `json:"usage,omitempty"`
	Error                 jdError `json:"error,omitempty"`
}

type usage struct {
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type jdError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
