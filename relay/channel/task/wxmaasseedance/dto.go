package wxmaasseedance

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

type mediaURL struct {
	URL string `json:"url"`
}

type canonicalContentItem struct {
	Type string
	Text string
	URL  string
}

type providerContentItem struct {
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
	Video         string         `json:"video,omitempty"`
	Videos        []string       `json:"videos,omitempty"`
	Audio         string         `json:"audio,omitempty"`
	Audios        []string       `json:"audios,omitempty"`
	Duration      *int           `json:"duration,omitempty"`
	Seconds       *string        `json:"seconds,omitempty"`
	Ratio         *string        `json:"ratio,omitempty"`
	Resolution    *string        `json:"resolution,omitempty"`
	GenerateAudio *bool          `json:"generate_audio,omitempty"`
	Watermark     *bool          `json:"watermark,omitempty"`
	Seed          *int           `json:"seed,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`

	ReturnLastFrame       *bool           `json:"return_last_frame,omitempty"`
	CallbackURL           *string         `json:"callback_url,omitempty"`
	Tools                 json.RawMessage `json:"tools,omitempty"`
	SafetyIdentifier      *string         `json:"safety_identifier,omitempty"`
	Draft                 *bool           `json:"draft,omitempty"`
	ExecutionExpiresAfter *int            `json:"execution_expires_after,omitempty"`
}

var allowedSubmitRequestFields = map[string]struct{}{
	"model": {}, "prompt": {}, "messages": {},
	"image": {}, "images": {}, "video": {}, "videos": {}, "audio": {}, "audios": {},
	"duration": {}, "seconds": {}, "ratio": {}, "resolution": {},
	"generate_audio": {}, "watermark": {}, "seed": {}, "metadata": {},
	"return_last_frame": {}, "callback_url": {}, "tools": {}, "safety_identifier": {},
	"draft": {}, "execution_expires_after": {},
}

func (r *submitRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return err
	}
	unknownFields := make([]string, 0)
	for field := range fields {
		if _, ok := allowedSubmitRequestFields[field]; !ok {
			unknownFields = append(unknownFields, field)
		}
	}
	if len(unknownFields) > 0 {
		sort.Strings(unknownFields)
		return fmt.Errorf("unsupported request field: %s", strings.Join(unknownFields, ", "))
	}
	for _, field := range forbiddenProviderFields {
		if _, exists := fields[field]; exists {
			return fmt.Errorf("%s is not supported", field)
		}
	}

	type plainSubmitRequest submitRequest
	var decoded plainSubmitRequest
	if err := common.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = submitRequest(decoded)
	return nil
}

type createRequest struct {
	Model           string                `json:"model"`
	Content         []providerContentItem `json:"content"`
	Resolution      string                `json:"resolution"`
	Ratio           string                `json:"ratio"`
	Duration        int                   `json:"duration"`
	GenerateAudio   bool                  `json:"generate_audio"`
	Watermark       bool                  `json:"watermark"`
	ReturnLastFrame bool                  `json:"return_last_frame"`
	Seed            *int                  `json:"seed,omitempty"`
}

type createResponse struct {
	TaskID string `json:"task_id"`
}

type createSnapshot struct {
	SchemaVersion  int    `json:"schema_version"`
	ProviderStatus string `json:"provider_status"`
	ArchiveState   string `json:"archive_state"`
}

type queryResponse struct {
	ID              string         `json:"id"`
	TaskID          string         `json:"task_id"`
	Model           string         `json:"model"`
	Status          string         `json:"status"`
	Content         queryContent   `json:"content"`
	Usage           *usage         `json:"usage,omitempty"`
	Error           *providerError `json:"error,omitempty"`
	Seed            *int64         `json:"seed,omitempty"`
	Resolution      string         `json:"resolution,omitempty"`
	Ratio           string         `json:"ratio,omitempty"`
	Duration        int            `json:"duration,omitempty"`
	FramesPerSecond int            `json:"frames_per_second,omitempty"`
	Framespersecond int            `json:"framespersecond,omitempty"`
	GenerateAudio   *bool          `json:"generate_audio,omitempty"`
	CreatedAt       int64          `json:"created_at,omitempty"`
	UpdatedAt       int64          `json:"updated_at,omitempty"`
}

type queryContent struct {
	VideoURL     string `json:"video_url,omitempty"`
	LastFrameURL string `json:"last_frame_url,omitempty"`
}

type usage struct {
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type providerError struct {
	Code string `json:"code,omitempty"`
}

type storedTaskMetadata struct {
	Seed            *int64
	Resolution      string
	Ratio           string
	Duration        int
	FramesPerSecond int
	GenerateAudio   *bool
}
