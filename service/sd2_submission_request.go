package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"golang.org/x/text/unicode/norm"
)

const (
	SD2OriginModel                     = "verdantflare-sd2"
	SD2RequestDigestVersion            = "sd2-public-hmac-v1"
	SD2SubmissionLedgerEnv             = "SD2_SUBMISSION_LEDGER_ENABLED"
	SD2BatchUpdateEnv                  = "BATCH_UPDATE_ENABLED"
	SD2RequestIdentityHMACKeyEnv       = "SD2_REQUEST_IDENTITY_HMAC_KEY_V1"
	SD2RequestIdentityHMACKeyMinLength = 32
	SD2DefaultDuration                 = 10
	SD2DefaultRatio                    = "16:9"
	SD2FixedResolution                 = "720P"
	SD2DefaultSeed                     = -1
)

// SD2SubmissionLedgerEnabled is the Release N/N+1 cutover gate. It defaults
// to false so an expand-only dark release cannot interrupt the existing JD
// path before the private archive and result-host allowlist are installed.
func SD2SubmissionLedgerEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(SD2SubmissionLedgerEnv)))
	return err == nil && enabled
}

// ConfigureSD2SubmissionIdentityFromEnv validates the immutable, versioned
// request-identity key before serving traffic. The ledger must never inherit
// common.CryptoSecret because that value may be generated at process start or
// rotated for unrelated application concerns.
func ConfigureSD2SubmissionIdentityFromEnv() error {
	rawEnabled := strings.TrimSpace(os.Getenv(SD2SubmissionLedgerEnv))
	enabled := false
	if rawEnabled != "" {
		parsed, err := strconv.ParseBool(rawEnabled)
		if err != nil {
			return fmt.Errorf("%s must be a boolean", SD2SubmissionLedgerEnv)
		}
		enabled = parsed
	}
	if enabled {
		batchRaw := strings.TrimSpace(os.Getenv(SD2BatchUpdateEnv))
		if batchRaw != "" {
			batchEnabled, err := strconv.ParseBool(batchRaw)
			if err != nil {
				return fmt.Errorf("%s must be a boolean when %s=true", SD2BatchUpdateEnv, SD2SubmissionLedgerEnv)
			}
			if batchEnabled {
				return fmt.Errorf("%s must be false when %s=true", SD2BatchUpdateEnv, SD2SubmissionLedgerEnv)
			}
		}
	}
	key := os.Getenv(SD2RequestIdentityHMACKeyEnv)
	if key == "" {
		if enabled {
			return fmt.Errorf("%s is required when %s=true", SD2RequestIdentityHMACKeyEnv, SD2SubmissionLedgerEnv)
		}
		return nil
	}
	if len([]byte(key)) < SD2RequestIdentityHMACKeyMinLength {
		return fmt.Errorf("%s must contain at least %d bytes", SD2RequestIdentityHMACKeyEnv, SD2RequestIdentityHMACKeyMinLength)
	}
	return nil
}

var sd2AllowedRatios = map[string]struct{}{
	"16:9": {},
	"9:16": {},
	"1:1":  {},
	"4:3":  {},
	"3:4":  {},
}

var errSD2ConflictingIntegerAliases = errors.New("conflicting integer aliases")

var ErrSD2SubmissionIdentityUnavailable = errors.New("sd2 submission identity is unavailable")

var sd2AllowedRootRequestFields = map[string]struct{}{
	"model": {}, "prompt": {}, "messages": {},
	"image": {}, "images": {}, "video": {}, "videos": {}, "audio": {}, "audios": {},
	"duration": {}, "seconds": {}, "ratio": {}, "resolution": {},
	"generate_audio": {}, "watermark": {}, "seed": {}, "metadata": {},
}

var sd2AllowedMetadataFields = map[string]struct{}{
	"content": {}, "duration": {}, "seconds": {}, "ratio": {}, "resolution": {},
	"generate_audio": {}, "watermark": {}, "seed": {},
}

var sd2ProviderControlledFields = []string{
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
	"provider",
	"provider_id",
	"channel",
	"channel_id",
	"upstream_model",
	"upstream_model_name",
}

// SD2NormalizedCreate is the provider-independent representation used by the
// submission fence. CanonicalJSON contains HMAC identities rather than prompt
// text or full media URLs and must never be treated as a recoverable request.
type SD2NormalizedCreate struct {
	RequestDigest  string
	DigestVersion  string
	RequestSummary string
	CanonicalJSON  []byte
	Duration       int
	Ratio          string
	Resolution     string
	GenerateAudio  bool
	Watermark      bool
	Seed           int
	ImageCount     int
	AudioCount     int
	VideoCount     int
}

// SD2CreateRequestError is safe to return to a public client.
type SD2CreateRequestError struct {
	Code    string
	Message string
}

func (e *SD2CreateRequestError) Error() string {
	return e.Message
}

// NormalizeSD2CreateRequest creates the T0 digest before routing. The input is
// decoded JSON so all encoding still goes through common.Marshal/Unmarshal.
// The canonical form intentionally excludes provider, channel, trace and
// upstream-model facts.
func NormalizeSD2CreateRequest(request map[string]any, action string) (*SD2NormalizedCreate, error) {
	modelName, _ := request["model"].(string)
	if nfcTrim(modelName) != SD2OriginModel {
		return nil, &SD2CreateRequestError{Code: "invalid_model", Message: "model must be verdantflare-sd2"}
	}
	if action == "" {
		action = "generate"
	}
	if action != "generate" {
		return nil, &SD2CreateRequestError{Code: "unsupported_action", Message: "verdantflare-sd2 supports video creation only"}
	}

	metadata, err := sd2RequestMetadata(request)
	if err != nil {
		return nil, err
	}
	if err := validateSD2PublicRequestShape(request, metadata); err != nil {
		return nil, err
	}
	identityKey, err := sd2RequestIdentityHMACKey()
	if err != nil {
		return nil, err
	}
	duration, err := sd2IntOption(request, metadata, []string{"duration", "seconds"}, SD2DefaultDuration)
	if errors.Is(err, errSD2ConflictingIntegerAliases) {
		return nil, &SD2CreateRequestError{Code: "conflicting_duration", Message: "duration and seconds must describe the same value"}
	}
	if err != nil || duration < 1 || duration > 15 {
		return nil, &SD2CreateRequestError{Code: "invalid_duration", Message: "duration must be an integer between 1 and 15"}
	}
	ratio := SD2DefaultRatio
	if value, ok := sd2OptionValue(request, metadata, "ratio"); ok {
		text, ok := value.(string)
		if !ok {
			return nil, &SD2CreateRequestError{Code: "invalid_ratio", Message: "ratio must be a string"}
		}
		ratio = nfcTrim(text)
	}
	if _, ok := sd2AllowedRatios[ratio]; !ok {
		return nil, &SD2CreateRequestError{Code: "invalid_ratio", Message: "unsupported video ratio"}
	}
	resolution := SD2FixedResolution
	if value, ok := sd2OptionValue(request, metadata, "resolution"); ok {
		text, ok := value.(string)
		if !ok || !strings.EqualFold(nfcTrim(text), SD2FixedResolution) {
			return nil, &SD2CreateRequestError{Code: "unsupported_resolution", Message: "verdantflare-sd2 currently supports 720P only"}
		}
	}
	generateAudio, err := sd2BoolOption(request, metadata, "generate_audio", true)
	if err != nil {
		return nil, &SD2CreateRequestError{Code: "invalid_generate_audio", Message: "generate_audio must be a boolean"}
	}
	watermark, err := sd2BoolOption(request, metadata, "watermark", false)
	if err != nil {
		return nil, &SD2CreateRequestError{Code: "invalid_watermark", Message: "watermark must be a boolean"}
	}
	seed, err := sd2IntOption(request, metadata, []string{"seed"}, SD2DefaultSeed)
	if err != nil {
		return nil, &SD2CreateRequestError{Code: "invalid_seed", Message: "seed must be an integer"}
	}

	messages, counts, err := sd2CanonicalMessages(request, identityKey)
	if err != nil {
		return nil, err
	}
	if counts["image"] > 9 || counts["video"] > 3 || counts["audio"] > 1 {
		return nil, &SD2CreateRequestError{Code: "media_limit_exceeded", Message: "verdantflare-sd2 supports up to 9 images, 3 videos, and 1 audio reference"}
	}

	canonical := map[string]any{
		"action":         action,
		"duration":       duration,
		"generate_audio": generateAudio,
		"messages":       messages,
		"origin_model":   SD2OriginModel,
		"ratio":          ratio,
		"resolution":     resolution,
		"schema_version": 1,
		"seed":           seed,
		"watermark":      watermark,
	}
	canonicalJSON, err := common.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical SD2 request: %w", err)
	}
	summaryJSON, err := common.Marshal(map[string]any{
		"audio_count":    counts["audio"],
		"duration":       duration,
		"generate_audio": generateAudio,
		"image_count":    counts["image"],
		"ratio":          ratio,
		"resolution":     resolution,
		"schema_version": 1,
		"seed":           seed,
		"video_count":    counts["video"],
		"watermark":      watermark,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal SD2 request summary: %w", err)
	}

	return &SD2NormalizedCreate{
		RequestDigest:  common.GenerateHMACWithKey(identityKey, "sd2-public-request-v1\x00"+string(canonicalJSON)),
		DigestVersion:  SD2RequestDigestVersion,
		RequestSummary: string(summaryJSON),
		CanonicalJSON:  canonicalJSON,
		Duration:       duration,
		Ratio:          ratio,
		Resolution:     resolution,
		GenerateAudio:  generateAudio,
		Watermark:      watermark,
		Seed:           seed,
		ImageCount:     counts["image"],
		AudioCount:     counts["audio"],
		VideoCount:     counts["video"],
	}, nil
}

func sd2CanonicalMessages(request map[string]any, identityKey []byte) ([]any, map[string]int, error) {
	counts := map[string]int{"image": 0, "audio": 0, "video": 0}
	metadata, err := sd2RequestMetadata(request)
	if err != nil {
		return nil, counts, err
	}
	_, hasMessages := request["messages"]
	_, hasMetadataContent := metadata["content"]
	if hasMessages && hasMetadataContent {
		return nil, counts, &SD2CreateRequestError{Code: "conflicting_content", Message: "messages and metadata.content cannot be used together"}
	}

	content := make([]any, 0)
	if hasMessages {
		messages, ok := request["messages"].([]any)
		if !ok || len(messages) == 0 {
			return nil, counts, &SD2CreateRequestError{Code: "invalid_messages", Message: "messages must be a non-empty array"}
		}
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				return nil, counts, &SD2CreateRequestError{Code: "invalid_messages", Message: "each message must be an object"}
			}
			role, _ := message["role"].(string)
			role = nfcTrim(role)
			if role != "" && role != "user" {
				continue
			}
			items, itemErr := sd2CanonicalContentItems(message["content"], counts, identityKey)
			if itemErr != nil {
				return nil, counts, itemErr
			}
			content = append(content, items...)
		}
	} else if hasMetadataContent {
		items, itemErr := sd2CanonicalContentItems(metadata["content"], counts, identityKey)
		if itemErr != nil {
			return nil, counts, itemErr
		}
		content = append(content, items...)
	}

	legacyItems, err := sd2CanonicalLegacyMedia(request, counts, identityKey)
	if err != nil {
		return nil, counts, err
	}
	content = append(content, legacyItems...)
	prompt, _ := request["prompt"].(string)
	prompt = nfcTrim(prompt)
	if !sd2HasTextContent(content) && prompt != "" {
		content = append([]any{map[string]any{"text_hmac": sensitiveIdentity(identityKey, "text", prompt), "type": "text"}}, content...)
	}
	if len(content) == 0 || sd2ContentType(content[0]) != "text" {
		return nil, counts, &SD2CreateRequestError{Code: "missing_prompt", Message: "the first content item must be non-empty text"}
	}
	return []any{map[string]any{"content": content, "role": "user"}}, counts, nil
}

func sd2CanonicalContentItems(raw any, counts map[string]int, identityKey []byte) ([]any, error) {
	if text, ok := raw.(string); ok {
		text = nfcTrim(text)
		if text == "" {
			return nil, &SD2CreateRequestError{Code: "invalid_content", Message: "text content must not be empty"}
		}
		return []any{map[string]any{"text_hmac": sensitiveIdentity(identityKey, "text", text), "type": "text"}}, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, &SD2CreateRequestError{Code: "invalid_content", Message: "content must be a non-empty array or string"}
	}
	content := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, &SD2CreateRequestError{Code: "invalid_content", Message: "each content item must be an object"}
		}
		itemType := strings.ToLower(nfcTrim(common.Interface2String(item["type"])))
		switch itemType {
		case "text":
			text := nfcTrim(common.Interface2String(item["text"]))
			if text == "" {
				continue
			}
			content = append(content, map[string]any{"text_hmac": sensitiveIdentity(identityKey, "text", text), "type": "text"})
		case "image_url", "video_url", "audio_url":
			mediaURL, err := sd2MediaURL(item[itemType])
			if err != nil {
				return nil, err
			}
			kind := strings.TrimSuffix(itemType, "_url")
			counts[kind]++
			canonical := map[string]any{"asset_hmac": sd2AssetIdentity(identityKey, mediaURL), "type": itemType}
			if role := nfcTrim(common.Interface2String(item["role"])); role != "" {
				canonical["role"] = role
			}
			content = append(content, canonical)
		default:
			return nil, &SD2CreateRequestError{Code: "unsupported_content_type", Message: "unsupported SD2 content type"}
		}
	}
	return content, nil
}

func sd2CanonicalLegacyMedia(request map[string]any, counts map[string]int, identityKey []byte) ([]any, error) {
	content := make([]any, 0)
	for _, kind := range []string{"image", "video", "audio"} {
		values := make([]string, 0)
		if value, exists := request[kind]; exists {
			text, ok := value.(string)
			if !ok {
				return nil, &SD2CreateRequestError{Code: "invalid_media_url", Message: kind + " must be an HTTPS URL string"}
			}
			if nfcTrim(text) != "" {
				values = append(values, text)
			}
		}
		if rawValues, exists := request[kind+"s"]; exists {
			list, ok := rawValues.([]any)
			if !ok {
				return nil, &SD2CreateRequestError{Code: "invalid_media_url", Message: kind + "s must be an array of HTTPS URL strings"}
			}
			for _, raw := range list {
				text, ok := raw.(string)
				if !ok {
					return nil, &SD2CreateRequestError{Code: "invalid_media_url", Message: kind + "s must contain only HTTPS URL strings"}
				}
				if nfcTrim(text) != "" {
					values = append(values, text)
				}
			}
		}
		for _, value := range values {
			mediaURL, err := sd2MediaURL(value)
			if err != nil {
				return nil, err
			}
			counts[kind]++
			content = append(content, map[string]any{"asset_hmac": sd2AssetIdentity(identityKey, mediaURL), "type": kind + "_url"})
		}
	}
	return content, nil
}

func sd2HasTextContent(content []any) bool {
	for _, item := range content {
		if sd2ContentType(item) == "text" {
			return true
		}
	}
	return false
}

func sd2ContentType(raw any) string {
	item, _ := raw.(map[string]any)
	return common.Interface2String(item["type"])
}

func sd2MediaURL(raw any) (string, error) {
	var value string
	switch typed := raw.(type) {
	case string:
		value = typed
	case map[string]any:
		value, _ = typed["url"].(string)
	}
	value = nfcTrim(value)
	parsed, err := url.Parse(value)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", &SD2CreateRequestError{Code: "invalid_media_url", Message: "media references must be credential-free HTTPS URLs"}
	}
	return value, nil
}

type sd2MediaResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ValidateSD2CreateMediaAddresses enforces the SD2 public-input network policy
// independently of the operator's generic fetch toggle. It runs only for a new
// submission (after the idempotency replay lookup and before T1), so a later
// DNS change cannot prevent recovery of an already-recorded intent.
func ValidateSD2CreateMediaAddresses(ctx context.Context, request map[string]any) error {
	return validateSD2CreateMediaAddresses(ctx, request, net.DefaultResolver)
}

func validateSD2CreateMediaAddresses(ctx context.Context, request map[string]any, resolver sd2MediaResolver) error {
	mediaURLs, err := sd2InputMediaURLs(request)
	if err != nil {
		return err
	}
	if len(mediaURLs) == 0 {
		return nil
	}
	if resolver == nil {
		return &SD2CreateRequestError{Code: "invalid_media_url", Message: "media reference host validation is unavailable"}
	}

	validationCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	protection := &common.SSRFProtection{
		AllowPrivateIp:         false,
		DomainFilterMode:       false,
		IpFilterMode:           false,
		AllowedPorts:           []int{443},
		ApplyIPFilterForDomain: true,
	}
	resolvedHosts := make(map[string][]net.IPAddr)
	for _, mediaURL := range mediaURLs {
		parsed, parseErr := url.Parse(mediaURL)
		if parseErr != nil {
			return invalidSD2MediaAddressError()
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "" || parsed.Port() != "" && parsed.Port() != "443" {
			return invalidSD2MediaAddressError()
		}
		if err := protection.ValidateNetworkTarget(host, 443); err != nil {
			return invalidSD2MediaAddressError()
		}
		if net.ParseIP(host) != nil {
			continue
		}
		addresses, found := resolvedHosts[host]
		if !found {
			addresses, err = resolver.LookupIPAddr(validationCtx, host)
			if err != nil || len(addresses) == 0 {
				return invalidSD2MediaAddressError()
			}
			resolvedHosts[host] = addresses
		}
		for _, address := range addresses {
			if address.IP == nil || protection.ValidateResolvedIP(host, address.IP) != nil {
				return invalidSD2MediaAddressError()
			}
		}
	}
	return nil
}

func invalidSD2MediaAddressError() error {
	return &SD2CreateRequestError{Code: "invalid_media_url", Message: "media references must resolve to public HTTPS addresses on port 443"}
}

func sd2InputMediaURLs(request map[string]any) ([]string, error) {
	urls := make([]string, 0)
	metadata, err := sd2RequestMetadata(request)
	if err != nil {
		return nil, err
	}
	appendContentURLs := func(raw any) error {
		items, ok := raw.([]any)
		if !ok {
			return nil
		}
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			itemType := strings.ToLower(nfcTrim(common.Interface2String(item["type"])))
			if itemType != "image_url" && itemType != "video_url" && itemType != "audio_url" {
				continue
			}
			mediaURL, mediaErr := sd2MediaURL(item[itemType])
			if mediaErr != nil {
				return mediaErr
			}
			urls = append(urls, mediaURL)
		}
		return nil
	}
	if rawMessages, ok := request["messages"].([]any); ok {
		for _, rawMessage := range rawMessages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			role := nfcTrim(common.Interface2String(message["role"]))
			if role != "" && role != "user" {
				continue
			}
			if err := appendContentURLs(message["content"]); err != nil {
				return nil, err
			}
		}
	} else if rawContent, ok := metadata["content"]; ok {
		if err := appendContentURLs(rawContent); err != nil {
			return nil, err
		}
	}

	for _, kind := range []string{"image", "video", "audio"} {
		if raw, exists := request[kind]; exists && nfcTrim(common.Interface2String(raw)) != "" {
			mediaURL, mediaErr := sd2MediaURL(raw)
			if mediaErr != nil {
				return nil, mediaErr
			}
			urls = append(urls, mediaURL)
		}
		if rawList, exists := request[kind+"s"]; exists {
			list, _ := rawList.([]any)
			for _, raw := range list {
				if nfcTrim(common.Interface2String(raw)) == "" {
					continue
				}
				mediaURL, mediaErr := sd2MediaURL(raw)
				if mediaErr != nil {
					return nil, mediaErr
				}
				urls = append(urls, mediaURL)
			}
		}
	}
	return urls, nil
}

func sd2AssetIdentity(identityKey []byte, rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return sensitiveIdentity(identityKey, "asset", rawURL)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	// Query bytes remain part of the identity, including on operator-owned
	// hosts. They can only be removed after a stable object-version resolver
	// contributes bucket/key plus version, checksum/ETag and size.
	return sensitiveIdentity(identityKey, "asset", parsed.String())
}

func sd2RequestIdentityHMACKey() ([]byte, error) {
	key := os.Getenv(SD2RequestIdentityHMACKeyEnv)
	if len([]byte(key)) < SD2RequestIdentityHMACKeyMinLength {
		return nil, fmt.Errorf("%w: %s must contain at least %d bytes", ErrSD2SubmissionIdentityUnavailable, SD2RequestIdentityHMACKeyEnv, SD2RequestIdentityHMACKeyMinLength)
	}
	return []byte(key), nil
}

func sensitiveIdentity(identityKey []byte, kind, value string) string {
	return common.GenerateHMACWithKey(identityKey, "sd2-"+kind+"-v1\x00"+norm.NFC.String(value))
}

func nfcTrim(value string) string {
	return norm.NFC.String(strings.TrimSpace(value))
}

func sd2RequestMetadata(request map[string]any) (map[string]any, error) {
	raw, exists := request["metadata"]
	if !exists || raw == nil {
		return map[string]any{}, nil
	}
	metadata, ok := raw.(map[string]any)
	if !ok {
		return nil, &SD2CreateRequestError{Code: "invalid_metadata", Message: "metadata must be an object"}
	}
	return metadata, nil
}

func validateSD2PublicRequestShape(request map[string]any, metadata map[string]any) error {
	if err := rejectSD2ProviderControlledFields(request, metadata); err != nil {
		return err
	}
	if key := firstUnsupportedSD2Field(request, sd2AllowedRootRequestFields); key != "" {
		return &SD2CreateRequestError{Code: "unsupported_request_field", Message: key + " is not supported by verdantflare-sd2"}
	}
	if key := firstUnsupportedSD2Field(metadata, sd2AllowedMetadataFields); key != "" {
		return &SD2CreateRequestError{Code: "unsupported_metadata_field", Message: "metadata." + key + " is not supported by verdantflare-sd2"}
	}
	if rawMessages, exists := request["messages"]; exists {
		if err := validateSD2MessagesShape(rawMessages); err != nil {
			return err
		}
	}
	if rawContent, exists := metadata["content"]; exists {
		if err := validateSD2ContentShape(rawContent); err != nil {
			return err
		}
	}
	return nil
}

func rejectSD2ProviderControlledFields(request map[string]any, metadata map[string]any) error {
	for _, key := range sd2ProviderControlledFields {
		if _, exists := request[key]; exists {
			return &SD2CreateRequestError{Code: "unsupported_provider_field", Message: key + " is controlled by the service and cannot be supplied"}
		}
		if _, exists := metadata[key]; exists {
			return &SD2CreateRequestError{Code: "unsupported_provider_field", Message: "metadata." + key + " is controlled by the service and cannot be supplied"}
		}
	}
	return nil
}

func firstUnsupportedSD2Field(fields map[string]any, allowed map[string]struct{}) string {
	unsupported := make([]string, 0)
	for key := range fields {
		if _, ok := allowed[key]; !ok {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) == 0 {
		return ""
	}
	sort.Strings(unsupported)
	return unsupported[0]
}

func validateSD2MessagesShape(raw any) error {
	messages, ok := raw.([]any)
	if !ok {
		return nil
	}
	allowed := map[string]struct{}{"role": {}, "content": {}}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if key := firstUnsupportedSD2Field(message, allowed); key != "" {
			return &SD2CreateRequestError{Code: "unsupported_message_field", Message: "messages[]." + key + " is not supported by verdantflare-sd2"}
		}
		if err := validateSD2ContentShape(message["content"]); err != nil {
			return err
		}
	}
	return nil
}

func validateSD2ContentShape(raw any) error {
	if _, ok := raw.(string); ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.ToLower(nfcTrim(common.Interface2String(item["type"])))
		allowed := map[string]struct{}{"type": {}}
		switch itemType {
		case "text":
			allowed["text"] = struct{}{}
		case "image_url", "video_url":
			allowed[itemType] = struct{}{}
			allowed["role"] = struct{}{}
		case "audio_url":
			allowed[itemType] = struct{}{}
		default:
			// The canonical parser returns the stable unsupported-type error;
			// keep the shape pass focused on fields for recognized item types.
			continue
		}
		if key := firstUnsupportedSD2Field(item, allowed); key != "" {
			return &SD2CreateRequestError{Code: "unsupported_content_field", Message: "content[]." + key + " is not supported for " + itemType}
		}
		if itemType == "image_url" || itemType == "video_url" || itemType == "audio_url" {
			if media, ok := item[itemType].(map[string]any); ok {
				if key := firstUnsupportedSD2Field(media, map[string]struct{}{"url": {}}); key != "" {
					return &SD2CreateRequestError{Code: "unsupported_content_field", Message: "content[]." + itemType + "." + key + " is not supported"}
				}
			}
		}
	}
	return nil
}

func sd2OptionValue(request map[string]any, metadata map[string]any, key string) (any, bool) {
	if value, exists := request[key]; exists && value != nil {
		return value, true
	}
	value, exists := metadata[key]
	return value, exists && value != nil
}

func sd2BoolOption(request map[string]any, metadata map[string]any, key string, fallback bool) (bool, error) {
	value, ok := sd2OptionValue(request, metadata, key)
	if !ok || value == nil {
		return fallback, nil
	}
	parsed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be boolean", key)
	}
	return parsed, nil
}

func sd2IntOption(request map[string]any, metadata map[string]any, keys []string, fallback int) (int, error) {
	selected := fallback
	found := false
	for _, key := range keys {
		for _, source := range []map[string]any{request, metadata} {
			value, ok := source[key]
			if !ok || value == nil {
				continue
			}
			parsed, err := sd2IntegerValue(value, key)
			if err != nil {
				return 0, err
			}
			if found && parsed != selected {
				return 0, fmt.Errorf("%w: %s", errSD2ConflictingIntegerAliases, strings.Join(keys, "/"))
			}
			selected = parsed
			found = true
		}
	}
	if !found {
		return fallback, nil
	}
	return selected, nil
}

func sd2IntegerValue(value any, key string) (int, error) {
	switch typed := value.(type) {
	case float64:
		parsed := int(typed)
		if float64(parsed) != typed {
			return 0, fmt.Errorf("%s must be integer", key)
		}
		return parsed, nil
	case int:
		return typed, nil
	case string:
		return strconv.Atoi(strings.TrimSpace(typed))
	default:
		return 0, fmt.Errorf("%s must be integer", key)
	}
}
