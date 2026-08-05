package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

const (
	SD2TaskResultSnapshotVersion   = 1
	SD2ArchivedResultSchemaVersion = 1

	SD2ArchiveStateNone     = "NONE"
	SD2ArchiveStatePending  = "PENDING"
	SD2ArchiveStateArchived = "ARCHIVED"
	SD2ArchiveStateFailed   = "FAILED"

	// A 15-second generated video should be far smaller than this. The limit is
	// deliberately generous while still bounding provider and archive reads.
	MaxSD2ArchivedVideoBytes int64 = 512 << 20
)

var (
	ErrSD2ResultArchiveUnavailable = errors.New("sd2 private result archive is not configured")
	ErrInvalidSD2ArchivedResult    = errors.New("invalid sd2 archived result")
)

// SD2TaskUsageSnapshot is the provider-cost input persisted for reconciliation.
// Provider response bodies and signed result URLs must never be stored alongside it.
type SD2TaskUsageSnapshot struct {
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
}

// SD2TaskResultSnapshot is the only provider-result shape that ledger-managed
// SD2 tasks may persist in Task.Data. Keep additions versioned and allowlisted.
// In particular, this type intentionally has no provider video URL field.
type SD2TaskResultSnapshot struct {
	SchemaVersion     int                  `json:"schema_version"`
	ProviderStatus    string               `json:"provider_status,omitempty"`
	SafeErrorCode     string               `json:"safe_error_code,omitempty"`
	Seed              *int64               `json:"seed,omitempty"`
	Resolution        string               `json:"resolution,omitempty"`
	Duration          int                  `json:"duration,omitempty"`
	Ratio             string               `json:"ratio,omitempty"`
	FramesPerSecond   int                  `json:"frames_per_second,omitempty"`
	GenerateAudio     *bool                `json:"generate_audio,omitempty"`
	Usage             SD2TaskUsageSnapshot `json:"usage,omitempty"`
	ProviderCreatedAt int64                `json:"provider_created_at,omitempty"`
	ProviderUpdatedAt int64                `json:"provider_updated_at,omitempty"`
	ArchivedResult    *SD2ArchivedResult   `json:"archived_result,omitempty"`
	ArchiveState      string               `json:"archive_state"`
}

func NewSD2TaskResultSnapshot() SD2TaskResultSnapshot {
	return SD2TaskResultSnapshot{
		SchemaVersion: SD2TaskResultSnapshotVersion,
		ArchiveState:  SD2ArchiveStateNone,
	}
}

func DecodeSD2TaskResultSnapshot(data []byte) (SD2TaskResultSnapshot, error) {
	var snapshot SD2TaskResultSnapshot
	if len(data) == 0 {
		return snapshot, fmt.Errorf("empty sd2 task result snapshot")
	}
	var fields map[string]any
	if err := common.Unmarshal(data, &fields); err != nil {
		return snapshot, fmt.Errorf("decode sd2 task result snapshot: %w", err)
	}
	allowedFields := map[string]struct{}{
		"schema_version": {}, "provider_status": {}, "safe_error_code": {},
		"seed": {}, "resolution": {}, "duration": {}, "ratio": {},
		"frames_per_second": {}, "generate_audio": {}, "usage": {},
		"provider_created_at": {}, "provider_updated_at": {},
		"archived_result": {}, "archive_state": {},
	}
	for field := range fields {
		if _, ok := allowedFields[field]; !ok {
			return snapshot, fmt.Errorf("sd2 task result snapshot contains unknown field %q", field)
		}
	}
	if rawUsage, ok := fields["usage"].(map[string]any); ok {
		for field := range rawUsage {
			if field != "completion_tokens" && field != "total_tokens" {
				return snapshot, fmt.Errorf("sd2 task result usage contains unknown field %q", field)
			}
		}
	}
	if rawArchivedResult, ok := fields["archived_result"].(map[string]any); ok {
		allowedArchivedResultFields := map[string]struct{}{
			"schema_version": {}, "ref": {}, "version_id": {}, "sha256": {},
			"size": {}, "content_type": {}, "etag": {},
		}
		for field := range rawArchivedResult {
			if _, allowed := allowedArchivedResultFields[field]; !allowed {
				return snapshot, fmt.Errorf("sd2 archived result contains unknown field %q", field)
			}
		}
	}
	if err := common.Unmarshal(data, &snapshot); err != nil {
		return snapshot, fmt.Errorf("decode sd2 task result snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func EncodeSD2TaskResultSnapshot(snapshot SD2TaskResultSnapshot) ([]byte, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	return common.Marshal(snapshot)
}

func (s SD2TaskResultSnapshot) Validate() error {
	if s.SchemaVersion != SD2TaskResultSnapshotVersion {
		return fmt.Errorf("unsupported sd2 task result snapshot version %d", s.SchemaVersion)
	}
	if len(s.ProviderStatus) > 32 || len(s.SafeErrorCode) > 64 || len(s.Resolution) > 16 || len(s.Ratio) > 16 {
		return fmt.Errorf("sd2 task result snapshot contains an oversized field")
	}
	if s.Usage.CompletionTokens < 0 || s.Usage.TotalTokens < 0 {
		return fmt.Errorf("sd2 task result snapshot contains negative usage")
	}
	if s.Usage.TotalTokens < s.Usage.CompletionTokens {
		return fmt.Errorf("sd2 task result snapshot contains inconsistent usage")
	}
	if s.Duration < 0 || s.Duration > 15 || s.FramesPerSecond < 0 || s.FramesPerSecond > 240 || s.ProviderCreatedAt < 0 || s.ProviderUpdatedAt < 0 {
		return fmt.Errorf("sd2 task result snapshot contains invalid metadata")
	}
	switch s.ArchiveState {
	case SD2ArchiveStateNone, SD2ArchiveStatePending, SD2ArchiveStateFailed:
		if s.ArchivedResult != nil {
			return fmt.Errorf("sd2 archive descriptor is only valid for archived results")
		}
	case SD2ArchiveStateArchived:
		if s.ArchivedResult == nil {
			return fmt.Errorf("sd2 archived result descriptor is missing")
		}
		if err := validateSD2ArchivedResult(*s.ArchivedResult); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown sd2 archive state %q", s.ArchiveState)
	}
	return nil
}

// SD2ArchiveRequest contains the short-lived provider URL. Implementations
// must not persist or log SourceURL and must fetch it without forwarding any
// client, VerdantFlare, or provider authorization headers.
type SD2ArchiveRequest struct {
	PublicTaskID       string
	UserID             int
	SourceURL          string
	AllowedSourceHosts []string
}

// SD2ArchivedResult is the complete immutable descriptor for one verified
// version of an object in the private result store. Ref is an opaque
// sd2-result:// reference, never a public or signed URL. A descriptor is not
// valid unless every field is present and canonical.
type SD2ArchivedResult struct {
	SchemaVersion int    `json:"schema_version"`
	Ref           string `json:"ref"`
	VersionID     string `json:"version_id"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	ContentType   string `json:"content_type"`
	ETag          string `json:"etag"`
}

type SD2ArchivedObject struct {
	Body          io.ReadCloser
	SchemaVersion int
	Ref           string
	VersionID     string
	SHA256        string
	ContentType   string
	Size          int64
	ETag          string
}

// SD2ResultArchive is implemented by the deployment's private object store.
// The API intentionally has no default filesystem or public-URL fallback:
// without an implementation a provider success cannot become public completed.
type SD2ResultArchive interface {
	Archive(ctx context.Context, request SD2ArchiveRequest) (SD2ArchivedResult, error)
	Open(ctx context.Context, expected SD2ArchivedResult) (*SD2ArchivedObject, error)
}

var sd2ResultArchiveRegistry struct {
	sync.RWMutex
	archive SD2ResultArchive
}

func SetSD2ResultArchive(archive SD2ResultArchive) {
	sd2ResultArchiveRegistry.Lock()
	defer sd2ResultArchiveRegistry.Unlock()
	sd2ResultArchiveRegistry.archive = archive
}

func IsSD2ResultArchiveConfigured() bool {
	sd2ResultArchiveRegistry.RLock()
	defer sd2ResultArchiveRegistry.RUnlock()
	return sd2ResultArchiveRegistry.archive != nil
}

func ArchiveSD2ProviderResult(ctx context.Context, request SD2ArchiveRequest) (SD2ArchivedResult, error) {
	if err := validateSD2ArchiveSource(request.SourceURL, request.AllowedSourceHosts); err != nil {
		return SD2ArchivedResult{}, err
	}

	sd2ResultArchiveRegistry.RLock()
	archive := sd2ResultArchiveRegistry.archive
	sd2ResultArchiveRegistry.RUnlock()
	if archive == nil {
		return SD2ArchivedResult{}, ErrSD2ResultArchiveUnavailable
	}

	result, err := archive.Archive(ctx, request)
	if err != nil {
		return SD2ArchivedResult{}, err
	}
	if err := validateSD2ArchivedResult(result); err != nil {
		return SD2ArchivedResult{}, err
	}
	return result, nil
}

func OpenSD2ArchivedResult(ctx context.Context, expected SD2ArchivedResult) (*SD2ArchivedObject, error) {
	if err := validateSD2ArchivedResult(expected); err != nil {
		return nil, err
	}
	sd2ResultArchiveRegistry.RLock()
	archive := sd2ResultArchiveRegistry.archive
	sd2ResultArchiveRegistry.RUnlock()
	if archive == nil {
		return nil, ErrSD2ResultArchiveUnavailable
	}

	object, err := archive.Open(ctx, expected)
	if err != nil {
		return nil, err
	}
	if object == nil || object.Body == nil ||
		object.SchemaVersion != expected.SchemaVersion || object.Ref != expected.Ref ||
		object.VersionID != expected.VersionID || object.SHA256 != expected.SHA256 ||
		object.Size != expected.Size || object.ContentType != expected.ContentType ||
		object.ETag != expected.ETag {
		if object != nil && object.Body != nil {
			_ = object.Body.Close()
		}
		return nil, ErrInvalidSD2ArchivedResult
	}
	return object, nil
}

func ValidateSD2ArchivedResultRef(ref string) error {
	if ref == "" || len(ref) > 512 {
		return ErrInvalidSD2ArchivedResult
	}
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "sd2-result" || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.Hostname() != parsed.Host {
		return ErrInvalidSD2ArchivedResult
	}
	if strings.HasPrefix(parsed.Host, ".") || strings.HasSuffix(parsed.Host, ".") || strings.Contains(parsed.Host, "..") {
		return ErrInvalidSD2ArchivedResult
	}
	for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, "%2e") || strings.EqualFold(segment, "%2e%2e") {
			return ErrInvalidSD2ArchivedResult
		}
	}
	for _, r := range parsed.Host + parsed.Path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '/' {
			continue
		}
		return ErrInvalidSD2ArchivedResult
	}
	return nil
}

func validateSD2ArchiveSource(sourceURL string, allowedHosts []string) error {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("invalid sd2 provider result url")
	}
	host := strings.ToLower(parsed.Hostname())
	if !validSD2ArchiveHostname(host) {
		return fmt.Errorf("invalid sd2 provider result host")
	}
	authority := strings.ToLower(parsed.Host)
	if authority != host && authority != host+":443" {
		return fmt.Errorf("invalid sd2 provider result url authority")
	}
	if len(allowedHosts) == 0 {
		return fmt.Errorf("sd2 provider result host allowlist is empty")
	}
	hostAllowed := false
	for _, allowedHost := range allowedHosts {
		if strings.EqualFold(strings.TrimSpace(allowedHost), parsed.Hostname()) {
			hostAllowed = true
			break
		}
	}
	if !hostAllowed {
		return fmt.Errorf("sd2 provider result host is not allowed")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return fmt.Errorf("sd2 provider result url port is not allowed")
	}
	return nil
}

func validateSD2ArchivedResult(result SD2ArchivedResult) error {
	if result.SchemaVersion != SD2ArchivedResultSchemaVersion {
		return ErrInvalidSD2ArchivedResult
	}
	if err := ValidateSD2ArchivedResultRef(result.Ref); err != nil {
		return err
	}
	if !validSD2ArchiveOpaqueValue(result.VersionID, 1024) || strings.EqualFold(result.VersionID, "null") ||
		!validSD2ArchiveOpaqueValue(result.ETag, 512) {
		return ErrInvalidSD2ArchivedResult
	}
	if result.Size <= 0 || result.Size > MaxSD2ArchivedVideoBytes ||
		result.ContentType != normalizedSD2ContentType(result.ContentType) ||
		!isSupportedSD2StoredContentType(result.ContentType) {
		return ErrInvalidSD2ArchivedResult
	}
	checksum, err := hex.DecodeString(result.SHA256)
	if err != nil || len(checksum) != sha256.Size || result.SHA256 != strings.ToLower(result.SHA256) {
		return ErrInvalidSD2ArchivedResult
	}
	return nil
}

func validSD2ArchiveOpaqueValue(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func isAllowedVideoContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	return strings.HasPrefix(mediaType, "video/")
}
