package wxmaasseedance

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

const (
	metadataContentKey       = "__wxmaas_content"
	metadataRatioKey         = "__wxmaas_ratio"
	metadataResolutionKey    = "__wxmaas_resolution"
	metadataGenerateAudioKey = "__wxmaas_generate_audio"
	metadataWatermarkKey     = "__wxmaas_watermark"
	metadataSeedKey          = "__wxmaas_seed"
)

var forbiddenProviderFields = []string{
	"return_last_frame",
	"callback_url",
	"tools",
	"safety_identifier",
	"draft",
	"execution_expires_after",
	"service_tier",
	"priority",
	"frames_per_second",
	"framespersecond",
}

func normalizeSubmitRequest(req submitRequest) (relaycommon.TaskSubmitReq, error) {
	if strings.TrimSpace(req.Model) == "" {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("model field is required")
	}
	if strings.TrimSpace(req.Model) != PublicModel {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("model must be %s", PublicModel)
	}
	if err := rejectProviderControlledFields(req); err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}

	messageContent, err := contentItemsFromMessages(req.Messages)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	metadataContent, hasMetadataContent, err := contentItemsFromMetadata(req.Metadata)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	if len(messageContent) > 0 && hasMetadataContent {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("messages content and metadata.content cannot be used together")
	}

	content := messageContent
	if len(content) == 0 && hasMetadataContent {
		content = metadataContent
	}
	content = append(content, legacyMediaItems(req)...)

	prompt := strings.TrimSpace(req.Prompt)
	if !hasTextContent(content) && prompt != "" {
		content = append([]canonicalContentItem{{Type: contentTypeText, Text: prompt}}, content...)
	}
	content, err = normalizeAndOrderContent(content)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	if prompt == "" {
		prompt = textFromContent(content)
	}
	if prompt == "" {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("prompt or text content is required")
	}

	duration, err := resolveDuration(req)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	ratio, err := resolveStringOption(req.Ratio, req.Metadata, "ratio", defaultRatio)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	if _, ok := allowedRatios[ratio]; !ok {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("invalid ratio: %s", ratio)
	}
	resolution, err := resolveStringOption(req.Resolution, req.Metadata, "resolution", defaultResolution)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	resolution = strings.ToLower(resolution)
	if resolution != defaultResolution {
		return relaycommon.TaskSubmitReq{}, fmt.Errorf("resolution must be %s", defaultResolution)
	}
	generateAudio, err := resolveBoolOption(req.GenerateAudio, req.Metadata, "generate_audio", true)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	watermark, err := resolveBoolOption(req.Watermark, req.Metadata, "watermark", false)
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}
	seed, err := resolveOptionalInt(req.Seed, req.Metadata, "seed")
	if err != nil {
		return relaycommon.TaskSubmitReq{}, err
	}

	metadata := map[string]any{
		metadataContentKey:       content,
		metadataRatioKey:         ratio,
		metadataResolutionKey:    resolution,
		metadataGenerateAudioKey: generateAudio,
		metadataWatermarkKey:     watermark,
	}
	if seed != nil {
		metadata[metadataSeedKey] = seed
	}

	return relaycommon.TaskSubmitReq{
		Prompt:   prompt,
		Model:    PublicModel,
		Duration: duration,
		Seconds:  strconv.Itoa(duration),
		Metadata: metadata,
	}, nil
}

func convertToCreateRequest(req relaycommon.TaskSubmitReq, info *relaycommon.RelayInfo) (*createRequest, error) {
	content, ok := req.Metadata[metadataContentKey].([]canonicalContentItem)
	if !ok || len(content) == 0 {
		return nil, fmt.Errorf("normalized content is required")
	}

	upstreamModel := UpstreamModel
	if info != nil && info.ChannelMeta != nil && strings.TrimSpace(info.UpstreamModelName) != "" {
		upstreamModel = strings.TrimSpace(info.UpstreamModelName)
	}
	if upstreamModel == PublicModel {
		upstreamModel = UpstreamModel
	}
	if upstreamModel != UpstreamModel {
		return nil, fmt.Errorf("selected video channel model mapping is invalid")
	}
	if info != nil && info.ChannelMeta != nil {
		info.UpstreamModelName = upstreamModel
	}

	providerContent := make([]providerContentItem, 0, len(content))
	for _, item := range content {
		providerItem := providerContentItem{Type: item.Type}
		switch item.Type {
		case contentTypeText:
			providerItem.Text = item.Text
		case contentTypeImageURL:
			providerItem.ImageURL = &mediaURL{URL: item.URL}
		case contentTypeVideoURL:
			providerItem.VideoURL = &mediaURL{URL: item.URL}
		case contentTypeAudioURL:
			providerItem.AudioURL = &mediaURL{URL: item.URL}
		default:
			return nil, fmt.Errorf("unsupported content type: %s", item.Type)
		}
		providerContent = append(providerContent, providerItem)
	}
	resolution, resolutionOK := req.Metadata[metadataResolutionKey].(string)
	ratio, ratioOK := req.Metadata[metadataRatioKey].(string)
	generateAudio, generateAudioOK := req.Metadata[metadataGenerateAudioKey].(bool)
	watermark, watermarkOK := req.Metadata[metadataWatermarkKey].(bool)
	if !resolutionOK || !ratioOK || !generateAudioOK || !watermarkOK {
		return nil, fmt.Errorf("normalized video options are invalid")
	}

	request := &createRequest{
		Model:           upstreamModel,
		Content:         providerContent,
		Resolution:      resolution,
		Ratio:           ratio,
		Duration:        req.Duration,
		GenerateAudio:   generateAudio,
		Watermark:       watermark,
		ReturnLastFrame: false,
	}
	if seed, ok := req.Metadata[metadataSeedKey].(*int); ok && seed != nil {
		value := *seed
		request.Seed = &value
	}
	return request, nil
}

func rejectProviderControlledFields(req submitRequest) error {
	switch {
	case req.ReturnLastFrame != nil:
		return fmt.Errorf("return_last_frame is not supported")
	case req.CallbackURL != nil:
		return fmt.Errorf("callback_url is not supported")
	case len(req.Tools) > 0:
		return fmt.Errorf("tools is not supported")
	case req.SafetyIdentifier != nil:
		return fmt.Errorf("safety_identifier is not supported")
	case req.Draft != nil:
		return fmt.Errorf("draft is not supported")
	case req.ExecutionExpiresAfter != nil:
		return fmt.Errorf("execution_expires_after is not supported")
	}
	for _, field := range forbiddenProviderFields {
		if _, exists := req.Metadata[field]; exists {
			return fmt.Errorf("%s is not supported", field)
		}
	}
	return nil
}

func contentItemsFromMessages(messages []dto.Message) ([]canonicalContentItem, error) {
	items := make([]canonicalContentItem, 0)
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role != "" && role != "user" {
			continue
		}
		messageItems, err := contentItemsFromAny(message.Content)
		if err != nil {
			return nil, err
		}
		items = append(items, messageItems...)
	}
	return items, nil
}

func contentItemsFromMetadata(metadata map[string]any) ([]canonicalContentItem, bool, error) {
	raw, exists := metadata["content"]
	if !exists {
		return nil, false, nil
	}
	items, err := contentItemsFromAny(raw)
	return items, true, err
}

func contentItemsFromAny(raw any) ([]canonicalContentItem, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return nil, nil
		}
		return []canonicalContentItem{{Type: contentTypeText, Text: text}}, nil
	case []canonicalContentItem:
		return append([]canonicalContentItem(nil), value...), nil
	case []any:
		items := make([]canonicalContentItem, 0, len(value))
		for _, rawItem := range value {
			item, include, err := contentItemFromAny(rawItem)
			if err != nil {
				return nil, err
			}
			if include {
				items = append(items, item)
			}
		}
		return items, nil
	default:
		var values []any
		data, err := common.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid content: %w", err)
		}
		if err := common.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("invalid content: %w", err)
		}
		return contentItemsFromAny(values)
	}
}

func contentItemFromAny(raw any) (canonicalContentItem, bool, error) {
	itemMap, ok := raw.(map[string]any)
	if !ok {
		data, err := common.Marshal(raw)
		if err != nil {
			return canonicalContentItem{}, false, fmt.Errorf("invalid content item: %w", err)
		}
		if err := common.Unmarshal(data, &itemMap); err != nil {
			return canonicalContentItem{}, false, fmt.Errorf("invalid content item: %w", err)
		}
	}

	contentType := strings.TrimSpace(common.Interface2String(itemMap["type"]))
	switch contentType {
	case contentTypeText:
		text := strings.TrimSpace(common.Interface2String(itemMap["text"]))
		if text == "" {
			return canonicalContentItem{}, false, nil
		}
		return canonicalContentItem{Type: contentTypeText, Text: text}, true, nil
	case contentTypeImageURL, contentTypeVideoURL, contentTypeAudioURL:
		mediaURL := extractMediaURL(itemMap[contentType])
		if mediaURL == "" {
			return canonicalContentItem{}, false, fmt.Errorf("%s.url is required", contentType)
		}
		return canonicalContentItem{Type: contentType, URL: mediaURL}, true, nil
	default:
		return canonicalContentItem{}, false, fmt.Errorf("unsupported content type: %s", contentType)
	}
}

func legacyMediaItems(req submitRequest) []canonicalContentItem {
	items := make([]canonicalContentItem, 0, 3+len(req.Images)+len(req.Videos)+len(req.Audios))
	for _, value := range append(singleton(req.Image), req.Images...) {
		if value = strings.TrimSpace(value); value != "" {
			items = append(items, canonicalContentItem{Type: contentTypeImageURL, URL: value})
		}
	}
	for _, value := range append(singleton(req.Video), req.Videos...) {
		if value = strings.TrimSpace(value); value != "" {
			items = append(items, canonicalContentItem{Type: contentTypeVideoURL, URL: value})
		}
	}
	for _, value := range append(singleton(req.Audio), req.Audios...) {
		if value = strings.TrimSpace(value); value != "" {
			items = append(items, canonicalContentItem{Type: contentTypeAudioURL, URL: value})
		}
	}
	return items
}

func normalizeAndOrderContent(items []canonicalContentItem) ([]canonicalContentItem, error) {
	normalized := make([]canonicalContentItem, 0, len(items))
	firstTextIndex := -1
	imageCount := 0
	videoCount := 0
	audioCount := 0
	for _, item := range items {
		switch item.Type {
		case contentTypeText:
			item.Text = strings.TrimSpace(item.Text)
			if item.Text != "" {
				if firstTextIndex < 0 {
					firstTextIndex = len(normalized)
				}
				normalized = append(normalized, item)
			}
		case contentTypeImageURL, contentTypeVideoURL, contentTypeAudioURL:
			item.URL = strings.TrimSpace(item.URL)
			if err := validateMediaURL(item.URL); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", item.Type, err)
			}
			switch item.Type {
			case contentTypeImageURL:
				imageCount++
			case contentTypeVideoURL:
				videoCount++
			case contentTypeAudioURL:
				audioCount++
			}
			normalized = append(normalized, item)
		default:
			return nil, fmt.Errorf("unsupported content type: %s", item.Type)
		}
	}
	if firstTextIndex < 0 {
		return nil, fmt.Errorf("prompt or text content is required")
	}
	if imageCount > maxImageCount {
		return nil, fmt.Errorf("at most %d image references are supported", maxImageCount)
	}
	if videoCount > maxVideoCount {
		return nil, fmt.Errorf("at most %d video references are supported", maxVideoCount)
	}
	if audioCount > maxAudioCount {
		return nil, fmt.Errorf("at most %d audio reference is supported", maxAudioCount)
	}
	if firstTextIndex == 0 {
		return normalized, nil
	}
	ordered := make([]canonicalContentItem, 0, len(normalized))
	ordered = append(ordered, normalized[firstTextIndex])
	ordered = append(ordered, normalized[:firstTextIndex]...)
	ordered = append(ordered, normalized[firstTextIndex+1:]...)
	return ordered, nil
}

func resolveDuration(req submitRequest) (int, error) {
	candidates := make([]any, 0, 4)
	if req.Duration != nil {
		candidates = append(candidates, *req.Duration)
	}
	if req.Seconds != nil {
		candidates = append(candidates, *req.Seconds)
	}
	if req.Metadata != nil {
		for _, key := range []string{"duration", "seconds"} {
			if value, ok := req.Metadata[key]; ok && value != nil {
				candidates = append(candidates, value)
			}
		}
	}
	if len(candidates) == 0 {
		return defaultDuration, nil
	}
	duration := 0
	for index, raw := range candidates {
		parsed, ok := intFromAny(raw)
		if !ok {
			return 0, fmt.Errorf("duration must be an integer")
		}
		if index > 0 && parsed != duration {
			return 0, fmt.Errorf("duration and seconds must describe the same value")
		}
		duration = parsed
	}
	if duration < minDuration || duration > maxDuration {
		return 0, fmt.Errorf("duration must be between %d and %d seconds", minDuration, maxDuration)
	}
	return duration, nil
}

func resolveStringOption(topLevel *string, metadata map[string]any, key, fallback string) (string, error) {
	if topLevel != nil {
		value := strings.TrimSpace(*topLevel)
		if value == "" {
			return "", fmt.Errorf("%s must not be empty", key)
		}
		return value, nil
	}
	if raw, exists := metadata[key]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return strings.TrimSpace(value), nil
	}
	return fallback, nil
}

func resolveBoolOption(topLevel *bool, metadata map[string]any, key string, fallback bool) (bool, error) {
	if topLevel != nil {
		return *topLevel, nil
	}
	if raw, exists := metadata[key]; exists {
		value, ok := raw.(bool)
		if !ok {
			return false, fmt.Errorf("%s must be a boolean", key)
		}
		return value, nil
	}
	return fallback, nil
}

func resolveOptionalInt(topLevel *int, metadata map[string]any, key string) (*int, error) {
	if topLevel != nil {
		value := *topLevel
		return &value, nil
	}
	raw, exists := metadata[key]
	if !exists {
		return nil, nil
	}
	value, ok := intFromAny(raw)
	if !ok {
		return nil, fmt.Errorf("%s must be an integer", key)
	}
	return &value, nil
}

func intFromAny(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
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

func extractMediaURL(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		return strings.TrimSpace(common.Interface2String(value["url"]))
	case mediaURL:
		return strings.TrimSpace(value.URL)
	case *mediaURL:
		if value != nil {
			return strings.TrimSpace(value.URL)
		}
	}
	return ""
}

func validateMediaURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("URL is invalid")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("URL must use HTTPS")
	}
	if parsed.User != nil {
		return fmt.Errorf("URL user info is not allowed")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("URL fragment is not allowed")
	}
	return nil
}

func hasTextContent(items []canonicalContentItem) bool {
	for _, item := range items {
		if item.Type == contentTypeText && strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return false
}

func textFromContent(items []canonicalContentItem) string {
	texts := make([]string, 0)
	for _, item := range items {
		if item.Type == contentTypeText && strings.TrimSpace(item.Text) != "" {
			texts = append(texts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(texts, "\n")
}

func singleton(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{value}
}
