package jdseedance

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

func normalizeSubmitRequest(req submitRequest) (relaycommon.TaskSubmitReq, error) {
	metadata := cloneMetadata(req.Metadata)

	duration, hasDuration, err := resolveDurationAliases(req)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	delete(metadata, "seconds")
	if hasDuration {
		metadata["duration"] = duration
	}
	if strings.TrimSpace(req.Ratio) != "" {
		metadata["ratio"] = strings.TrimSpace(req.Ratio)
	}
	if req.GenerateAudio != nil {
		metadata["generate_audio"] = *req.GenerateAudio
	}
	if req.Watermark != nil {
		metadata["watermark"] = *req.Watermark
	}

	messageContent, messageText, err := contentItemsFromMessages(req.Messages)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	content := make([]contentItem, 0)
	if len(messageContent) > 0 {
		content = append(content, messageContent...)
	} else if rawContent, ok := metadata["content"]; ok {
		metadataContent, err := contentItemsFromAny(rawContent)
		if err != nil {
			return relaycommon.TaskSubmitReq{}, err
		}
		content = append(content, metadataContent...)
	}
	for _, image := range append(singleton(req.Image), req.Images...) {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		content = append(content, contentItem{
			Type:     contentTypeImageURL,
			ImageURL: &mediaURL{URL: image},
			Role:     defaultImageRole,
		})
	}
	for _, video := range append(singleton(req.Video), req.Videos...) {
		video = strings.TrimSpace(video)
		if video == "" {
			continue
		}
		content = append(content, contentItem{
			Type:     contentTypeVideoURL,
			VideoURL: &mediaURL{URL: video},
			Role:     defaultVideoRole,
		})
	}
	for _, audio := range append(singleton(req.Audio), req.Audios...) {
		audio = strings.TrimSpace(audio)
		if audio == "" {
			continue
		}
		content = append(content, contentItem{
			Type:     contentTypeAudioURL,
			AudioURL: &mediaURL{URL: audio},
		})
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(strings.Join(messageText, "\n"))
	}
	if prompt == "" {
		prompt = strings.TrimSpace(textFromContent(content))
	}
	if prompt != "" && !hasTextContent(content) {
		content = append([]contentItem{{Type: contentTypeText, Text: prompt}}, content...)
	}
	if len(content) == 0 {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("content is required")
	}
	if strings.TrimSpace(prompt) == "" {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("prompt or text content is required")
	}

	metadata["content"] = content

	return relaycommon.TaskSubmitReq{
		Prompt:   prompt,
		Model:    strings.TrimSpace(req.Model),
		Duration: intFromAny(metadata["duration"]),
		Seconds:  common.Interface2String(metadata["duration"]),
		Metadata: metadata,
	}, nil
}

func resolveDurationAliases(req submitRequest) (int, bool, error) {
	candidates := make([]any, 0, 4)
	if req.Duration > 0 {
		candidates = append(candidates, req.Duration)
	}
	if strings.TrimSpace(req.Seconds) != "" {
		candidates = append(candidates, req.Seconds)
	}
	for _, key := range []string{"duration", "seconds"} {
		if value, ok := req.Metadata[key]; ok && value != nil {
			candidates = append(candidates, value)
		}
	}
	if len(candidates) == 0 {
		return 0, false, nil
	}
	selected := 0
	for index, raw := range candidates {
		parsed, ok := strictIntFromAny(raw)
		if !ok {
			return 0, false, fmt.Errorf("duration must be an integer")
		}
		if index > 0 && parsed != selected {
			return 0, false, fmt.Errorf("duration and seconds must describe the same value")
		}
		selected = parsed
	}
	return selected, true, nil
}

func strictIntFromAny(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int64:
		maxInt := int64(^uint(0) >> 1)
		minInt := -maxInt - 1
		if value > maxInt || value < minInt {
			return 0, false
		}
		return int(value), true
	case float64:
		integerLimit := math.Ldexp(1, strconv.IntSize-1)
		if math.IsNaN(value) || math.IsInf(value, 0) || value < -integerLimit || value >= integerLimit || math.Trunc(value) != value {
			return 0, false
		}
		return int(value), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func convertToCreateRequest(req relaycommon.TaskSubmitReq) (*createRequest, error) {
	content, err := contentItemsFromAny(req.Metadata["content"])
	if err != nil {
		return nil, err
	}
	if len(content) == 0 && strings.TrimSpace(req.Prompt) != "" {
		content = append(content, contentItem{
			Type: contentTypeText,
			Text: strings.TrimSpace(req.Prompt),
		})
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("content is required")
	}

	ratio := strings.TrimSpace(common.Interface2String(req.Metadata["ratio"]))
	if ratio == "" {
		ratio = defaultRatio
	}
	if _, ok := allowedRatios[ratio]; !ok {
		return nil, fmt.Errorf("invalid ratio: %s", ratio)
	}

	duration := intFromAny(req.Metadata["duration"])
	if duration <= 0 {
		duration = req.Duration
	}
	if duration <= 0 && strings.TrimSpace(req.Seconds) != "" {
		duration = intFromAny(req.Seconds)
	}
	if duration <= 0 {
		return nil, fmt.Errorf("duration is required")
	}

	return &createRequest{
		Content:       content,
		GenerateAudio: boolFromAny(req.Metadata["generate_audio"], true),
		Ratio:         ratio,
		Duration:      duration,
		Watermark:     boolFromAny(req.Metadata["watermark"], false),
	}, nil
}

func cloneMetadata(metadata map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range metadata {
		out[k] = v
	}
	return out
}

func singleton(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return []string{v}
}

func contentItemsFromMessages(messages []dto.Message) ([]contentItem, []string, error) {
	items := make([]contentItem, 0)
	texts := make([]string, 0)
	for _, message := range messages {
		if message.Role != "" && message.Role != "user" {
			continue
		}
		messageItems, err := contentItemsFromMessageContent(message.Content)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range messageItems {
			if item.Type == contentTypeText && strings.TrimSpace(item.Text) != "" {
				texts = append(texts, strings.TrimSpace(item.Text))
			}
			items = append(items, item)
		}
	}
	return items, texts, nil
}

func contentItemsFromMessageContent(raw any) ([]contentItem, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil, nil
		}
		return []contentItem{{Type: contentTypeText, Text: text}}, nil
	case []any:
		items := make([]contentItem, 0, len(v))
		for _, rawItem := range v {
			item, ok, err := contentItemFromAny(rawItem)
			if err != nil {
				return nil, err
			}
			if ok {
				items = append(items, item)
			}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unsupported message content")
	}
}

func contentItemsFromAny(raw any) ([]contentItem, error) {
	if raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []contentItem:
		return normalizeContentItems(v)
	case []any:
		items := make([]contentItem, 0, len(v))
		for _, item := range v {
			content, ok, err := contentItemFromAny(item)
			if err != nil {
				return nil, err
			}
			if ok {
				items = append(items, content)
			}
		}
		return normalizeContentItems(items)
	default:
		var items []contentItem
		data, err := common.Marshal(raw)
		if err != nil {
			return nil, err
		}
		if err := common.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("invalid content: %w", err)
		}
		return normalizeContentItems(items)
	}
}

func contentItemFromAny(raw any) (contentItem, bool, error) {
	switch v := raw.(type) {
	case contentItem:
		items, err := normalizeContentItems([]contentItem{v})
		if err != nil || len(items) == 0 {
			return contentItem{}, false, err
		}
		return items[0], true, nil
	case map[string]any:
		return contentItemFromMap(v)
	default:
		var m map[string]any
		data, err := common.Marshal(raw)
		if err != nil {
			return contentItem{}, false, err
		}
		if err := common.Unmarshal(data, &m); err != nil {
			return contentItem{}, false, fmt.Errorf("invalid content item: %w", err)
		}
		return contentItemFromMap(m)
	}
}

func contentItemFromMap(m map[string]any) (contentItem, bool, error) {
	contentType := strings.TrimSpace(common.Interface2String(m["type"]))
	switch contentType {
	case contentTypeText:
		text := strings.TrimSpace(common.Interface2String(m["text"]))
		if text == "" {
			return contentItem{}, false, nil
		}
		return contentItem{Type: contentTypeText, Text: text}, true, nil
	case contentTypeImageURL:
		url := extractMediaURL(m[contentTypeImageURL])
		if url == "" {
			return contentItem{}, false, fmt.Errorf("image_url.url is required")
		}
		return contentItem{
			Type:     contentTypeImageURL,
			ImageURL: &mediaURL{URL: url},
			Role:     strings.TrimSpace(common.Interface2String(m["role"])),
		}, true, nil
	case contentTypeVideoURL:
		url := extractMediaURL(m[contentTypeVideoURL])
		if url == "" {
			return contentItem{}, false, fmt.Errorf("video_url.url is required")
		}
		return contentItem{
			Type:     contentTypeVideoURL,
			VideoURL: &mediaURL{URL: url},
			Role:     strings.TrimSpace(common.Interface2String(m["role"])),
		}, true, nil
	case contentTypeAudioURL:
		url := extractMediaURL(m[contentTypeAudioURL])
		if url == "" {
			return contentItem{}, false, fmt.Errorf("audio_url.url is required")
		}
		return contentItem{Type: contentTypeAudioURL, AudioURL: &mediaURL{URL: url}}, true, nil
	default:
		if url := extractMediaURL(m["video_url"]); url != "" {
			return contentItem{
				Type:     contentTypeVideoURL,
				VideoURL: &mediaURL{URL: url},
				Role:     strings.TrimSpace(common.Interface2String(m["role"])),
			}, true, nil
		}
		if url := extractMediaURL(m["audio_url"]); url != "" {
			return contentItem{Type: contentTypeAudioURL, AudioURL: &mediaURL{URL: url}}, true, nil
		}
		if url := extractMediaURL(m["image_url"]); url != "" {
			return contentItem{
				Type:     contentTypeImageURL,
				ImageURL: &mediaURL{URL: url},
				Role:     strings.TrimSpace(common.Interface2String(m["role"])),
			}, true, nil
		}
		return contentItem{}, false, fmt.Errorf("unsupported content type: %s", contentType)
	}
}

func normalizeContentItems(items []contentItem) ([]contentItem, error) {
	out := make([]contentItem, 0, len(items))
	videoCount := 0
	for _, item := range items {
		item.Type = strings.TrimSpace(item.Type)
		switch item.Type {
		case contentTypeText:
			item.Text = strings.TrimSpace(item.Text)
			if item.Text == "" {
				continue
			}
		case contentTypeImageURL:
			if item.ImageURL == nil || strings.TrimSpace(item.ImageURL.URL) == "" {
				return nil, fmt.Errorf("image_url.url is required")
			}
			item.ImageURL.URL = strings.TrimSpace(item.ImageURL.URL)
			item.Role = strings.TrimSpace(item.Role)
			if item.Role == "" {
				item.Role = defaultImageRole
			}
		case contentTypeVideoURL:
			if item.VideoURL == nil || strings.TrimSpace(item.VideoURL.URL) == "" {
				return nil, fmt.Errorf("video_url.url is required")
			}
			videoCount++
			if videoCount > maxVideoReferences {
				return nil, fmt.Errorf("at most %d video references are supported", maxVideoReferences)
			}
			item.VideoURL.URL = strings.TrimSpace(item.VideoURL.URL)
			item.Role = strings.TrimSpace(item.Role)
			if item.Role == "" {
				item.Role = defaultVideoRole
			}
			if item.Role != defaultVideoRole {
				return nil, fmt.Errorf("video_url.role must be %s", defaultVideoRole)
			}
		case contentTypeAudioURL:
			if item.AudioURL == nil || strings.TrimSpace(item.AudioURL.URL) == "" {
				return nil, fmt.Errorf("audio_url.url is required")
			}
			item.AudioURL.URL = strings.TrimSpace(item.AudioURL.URL)
		default:
			return nil, fmt.Errorf("unsupported content type: %s", item.Type)
		}
		out = append(out, item)
	}
	return out, nil
}

func extractMediaURL(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case mediaURL:
		return strings.TrimSpace(v.URL)
	case *mediaURL:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(v.URL)
	case map[string]any:
		return strings.TrimSpace(common.Interface2String(v["url"]))
	default:
		var m map[string]any
		data, err := common.Marshal(raw)
		if err != nil {
			return ""
		}
		if err := common.Unmarshal(data, &m); err != nil {
			return ""
		}
		return strings.TrimSpace(common.Interface2String(m["url"]))
	}
}

func hasTextContent(items []contentItem) bool {
	for _, item := range items {
		if item.Type == contentTypeText && strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return false
}

func textFromContent(items []contentItem) string {
	texts := make([]string, 0)
	for _, item := range items {
		if item.Type == contentTypeText && strings.TrimSpace(item.Text) != "" {
			texts = append(texts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(texts, "\n")
}

func intFromAny(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(v))
		return i
	default:
		i, _ := strconv.Atoi(common.Interface2String(v))
		return i
	}
}

func boolFromAny(raw any, fallback bool) bool {
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fallback
		}
		return b
	default:
		if raw == nil {
			return fallback
		}
		b, err := strconv.ParseBool(common.Interface2String(raw))
		if err != nil {
			return fallback
		}
		return b
	}
}
