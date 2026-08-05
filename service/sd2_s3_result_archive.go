package service

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	sd2ArchiveRefHost             = "s3"
	sd2ArchiveMetadataSchema      = "vf-schema"
	sd2ArchiveMetadataTask        = "vf-task"
	sd2ArchiveMetadataSource      = "vf-source"
	sd2ArchiveMetadataSHA256      = "vf-sha256"
	sd2ArchiveMetadataSize        = "vf-size"
	sd2ArchiveMetadataContentType = "vf-content-type"
	sd2ArchiveMetadataSSE         = "vf-sse"
	sd2ArchiveMetadataRetention   = "vf-retention-days"
	sd2ArchiveObjectSchemaVersion = "1"
	sd2ArchiveCacheControl        = "private, no-store"
	sd2ArchiveTempMarkerName      = ".verdantflare-sd2-archive-temp-v1"
	sd2ArchiveTempMarkerContents  = "verdantflare-sd2-archive-temp-v1\n"
	sd2ArchiveStaleTempAge        = 2 * time.Hour
)

type sd2S3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type sd2ArchiveHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type sd2S3ResultArchive struct {
	config       sd2S3ArchiveConfig
	client       sd2S3API
	sourceClient sd2ArchiveHTTPDoer
	tempDir      string
	tempCleanup  sync.Mutex
	maxVideoSize int64
}

type sd2ArchiveObjectDescriptor struct {
	key         string
	versionID   string
	contentType string
	size        int64
	sha256      string
	etag        string
}

type sd2ArchiveDownload struct {
	file        *os.File
	contentType string
	size        int64
	sha256      string
}

func newSD2S3ResultArchive(config sd2S3ArchiveConfig, client sd2S3API) (*sd2S3ResultArchive, error) {
	if client == nil || config.Endpoint == "" || config.Region == "" || config.Bucket == "" || config.Prefix == "" || config.RetentionDays <= 0 || len(config.IdentityHMACKey) < 32 || config.TempDir == "" {
		return nil, fmt.Errorf("invalid SD2 result archive configuration")
	}
	if config.SSE != types.ServerSideEncryptionAes256 && config.SSE != types.ServerSideEncryptionAwsKms {
		return nil, fmt.Errorf("invalid SD2 result archive encryption configuration")
	}
	if (config.SSE == types.ServerSideEncryptionAwsKms) != (strings.TrimSpace(config.KMSKeyID) != "") {
		return nil, fmt.Errorf("invalid SD2 result archive KMS configuration")
	}
	tempDir, err := prepareSD2ArchiveTempDir(config.TempDir, time.Now())
	if err != nil {
		return nil, err
	}
	sourceClient, err := newSD2ArchiveSourceHTTPClient(nil, nil)
	if err != nil {
		return nil, err
	}
	// The SDK credentials provider is the only component that needs these
	// values after construction. Do not retain a second copy in the archive or
	// in the result-host resolver closure.
	config.AccessKeyID = ""
	config.SecretAccessKey = ""
	config.SessionToken = ""
	config.ResultHosts = nil
	return &sd2S3ResultArchive{
		config:       config,
		client:       client,
		sourceClient: sourceClient,
		tempDir:      tempDir,
		maxVideoSize: MaxSD2ArchivedVideoBytes,
	}, nil
}

func prepareSD2ArchiveTempDir(configuredPath string, now time.Time) (string, error) {
	absolutePath, err := filepath.Abs(filepath.Clean(configuredPath))
	if err != nil || !filepath.IsAbs(absolutePath) || isFilesystemRoot(absolutePath) {
		return "", fmt.Errorf("prepare dedicated SD2 result archive temp directory")
	}
	if err := os.MkdirAll(absolutePath, 0o700); err != nil {
		return "", fmt.Errorf("prepare dedicated SD2 result archive temp directory")
	}
	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil || isFilesystemRoot(resolvedPath) {
		return "", fmt.Errorf("resolve dedicated SD2 result archive temp directory")
	}
	absolutePath = resolvedPath
	directoryInfo, err := os.Stat(absolutePath)
	if err != nil || !directoryInfo.IsDir() {
		return "", fmt.Errorf("invalid dedicated SD2 result archive temp directory")
	}
	entries, err := os.ReadDir(absolutePath)
	if err != nil {
		return "", fmt.Errorf("inspect dedicated SD2 result archive temp directory")
	}
	markerPath := filepath.Join(absolutePath, sd2ArchiveTempMarkerName)
	markerFound := false
	for _, entry := range entries {
		if entry.Name() == sd2ArchiveTempMarkerName {
			markerFound = true
			break
		}
	}
	if !markerFound && len(entries) != 0 {
		return "", fmt.Errorf("dedicated SD2 result archive temp directory is not owned by this service")
	}
	if err := os.Chmod(absolutePath, 0o700); err != nil {
		return "", fmt.Errorf("secure dedicated SD2 result archive temp directory")
	}
	if !markerFound {
		marker, createErr := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr == nil {
			_, writeErr := io.WriteString(marker, sd2ArchiveTempMarkerContents)
			closeErr := marker.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(markerPath)
				return "", fmt.Errorf("mark dedicated SD2 result archive temp directory")
			}
		} else if !os.IsExist(createErr) {
			return "", fmt.Errorf("mark dedicated SD2 result archive temp directory")
		}
	}
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("invalid dedicated SD2 result archive temp marker")
	}
	markerContents, err := os.ReadFile(markerPath)
	if err != nil || string(markerContents) != sd2ArchiveTempMarkerContents {
		return "", fmt.Errorf("invalid dedicated SD2 result archive temp marker")
	}
	if err := os.Chmod(markerPath, 0o600); err != nil {
		return "", fmt.Errorf("secure dedicated SD2 result archive temp marker")
	}
	if err := cleanupSD2ArchiveStaleTempFiles(absolutePath, now); err != nil {
		return "", err
	}
	return absolutePath, nil
}

func cleanupSD2ArchiveStaleTempFiles(tempDir string, now time.Time) error {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return fmt.Errorf("inspect SD2 result archive temp files")
	}
	cutoff := now.Add(-sd2ArchiveStaleTempAge)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "sd2-result-") || !strings.HasSuffix(name, ".video") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect SD2 result archive temp file")
		}
		if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(tempDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clean stale SD2 result archive temp file")
		}
	}
	return nil
}

func (a *sd2S3ResultArchive) cleanupStaleTempFiles() error {
	a.tempCleanup.Lock()
	defer a.tempCleanup.Unlock()
	return cleanupSD2ArchiveStaleTempFiles(a.tempDir, time.Now())
}

func (a *sd2S3ResultArchive) archiveVideoSizeLimit() int64 {
	if a.maxVideoSize <= 0 || a.maxVideoSize > MaxSD2ArchivedVideoBytes {
		return MaxSD2ArchivedVideoBytes
	}
	return a.maxVideoSize
}

func newSD2ArchiveSourceHTTPClient(resolver ssrfResolver, dialContext func(context.Context, string, string) (net.Conn, error)) (*http.Client, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	protection, err := common.NewSSRFProtectionFromFetchSetting(
		false,
		false,
		false,
		nil,
		nil,
		[]string{"443"},
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize SD2 result source protection")
	}
	protectedDialer := &protectedFetchDialer{
		resolver:    resolver,
		dialContext: dialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) {
			return protection, true, nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           protectedDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   20 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func (a *sd2S3ResultArchive) Archive(ctx context.Context, request SD2ArchiveRequest) (SD2ArchivedResult, error) {
	if err := validateSD2ArchiveSource(request.SourceURL, request.AllowedSourceHosts); err != nil {
		return SD2ArchivedResult{}, err
	}
	if request.UserID <= 0 || !validSD2ArchivePublicTaskID(request.PublicTaskID) {
		return SD2ArchivedResult{}, fmt.Errorf("invalid SD2 archive task identity")
	}

	taskIdentity := common.GenerateHMACWithKey(a.config.IdentityHMACKey, "sd2-archive-task-v1\x00"+strconv.Itoa(request.UserID)+"\x00"+request.PublicTaskID)
	sourceIdentity, err := sd2ArchiveSourceIdentity(a.config.IdentityHMACKey, request.SourceURL)
	if err != nil {
		return SD2ArchivedResult{}, err
	}
	key := a.objectKey(taskIdentity, sourceIdentity)

	existing, found, err := a.headVerifiedObject(ctx, key, taskIdentity, sourceIdentity, "")
	if err != nil {
		return SD2ArchivedResult{}, err
	}
	if found {
		return a.archivedResult(existing), nil
	}

	download, err := a.downloadSource(ctx, request.SourceURL, request.AllowedSourceHosts)
	if err != nil {
		return SD2ArchivedResult{}, err
	}
	defer func() {
		_ = download.file.Close()
		_ = os.Remove(download.file.Name())
	}()

	if _, err := download.file.Seek(0, io.SeekStart); err != nil {
		return SD2ArchivedResult{}, fmt.Errorf("prepare SD2 result archive upload")
	}
	metadata := map[string]string{
		sd2ArchiveMetadataSchema:      sd2ArchiveObjectSchemaVersion,
		sd2ArchiveMetadataTask:        taskIdentity,
		sd2ArchiveMetadataSource:      sourceIdentity,
		sd2ArchiveMetadataSHA256:      download.sha256,
		sd2ArchiveMetadataSize:        strconv.FormatInt(download.size, 10),
		sd2ArchiveMetadataContentType: download.contentType,
		sd2ArchiveMetadataSSE:         string(a.config.SSE),
		sd2ArchiveMetadataRetention:   strconv.Itoa(a.config.RetentionDays),
	}
	putInput := &s3.PutObjectInput{
		Bucket:               aws.String(a.config.Bucket),
		Key:                  aws.String(key),
		Body:                 download.file,
		CacheControl:         aws.String(sd2ArchiveCacheControl),
		ContentLength:        aws.Int64(download.size),
		ContentType:          aws.String(download.contentType),
		ExpectedBucketOwner:  optionalAWSString(a.config.ExpectedBucketOwner),
		IfNoneMatch:          aws.String("*"),
		Metadata:             metadata,
		ServerSideEncryption: a.config.SSE,
		SSEKMSKeyId:          optionalAWSString(a.config.KMSKeyID),
	}
	_, putErr := a.client.PutObject(ctx, putInput)
	if putErr != nil {
		// A timeout can happen after the object was committed, and concurrent
		// retries can lose the If-None-Match race. In either case, reuse only the
		// same deterministic key after every metadata field verifies.
		existing, found, headErr := a.headVerifiedObject(ctx, key, taskIdentity, sourceIdentity, "")
		if headErr != nil {
			return SD2ArchivedResult{}, headErr
		}
		if found {
			return a.archivedResult(existing), nil
		}
		return SD2ArchivedResult{}, fmt.Errorf("store SD2 result object")
	}

	stored, found, err := a.headVerifiedObject(ctx, key, taskIdentity, sourceIdentity, "")
	if err != nil {
		return SD2ArchivedResult{}, err
	}
	if !found {
		return SD2ArchivedResult{}, fmt.Errorf("verify SD2 result object")
	}
	return a.archivedResult(stored), nil
}

func (a *sd2S3ResultArchive) Open(ctx context.Context, expected SD2ArchivedResult) (*SD2ArchivedObject, error) {
	if err := validateSD2ArchivedResult(expected); err != nil {
		return nil, err
	}
	key, err := a.keyFromRef(expected.Ref)
	if err != nil {
		return nil, err
	}
	descriptor, found, err := a.headVerifiedObject(ctx, key, "", "", expected.VersionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrInvalidSD2ArchivedResult
	}
	if a.archivedResult(descriptor) != expected {
		return nil, ErrInvalidSD2ArchivedResult
	}

	getOutput, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:              aws.String(a.config.Bucket),
		Key:                 aws.String(key),
		ExpectedBucketOwner: optionalAWSString(a.config.ExpectedBucketOwner),
		IfMatch:             aws.String(expected.ETag),
		VersionId:           aws.String(expected.VersionID),
	})
	if err != nil || getOutput == nil || getOutput.Body == nil {
		if getOutput != nil && getOutput.Body != nil {
			_ = getOutput.Body.Close()
		}
		return nil, fmt.Errorf("open SD2 result object")
	}
	if err := a.validateGetObject(getOutput, descriptor); err != nil {
		_ = getOutput.Body.Close()
		return nil, err
	}

	return &SD2ArchivedObject{
		Body: &sd2VerifiedReadCloser{
			body:     getOutput.Body,
			expected: descriptor.size,
			checksum: descriptor.sha256,
			hash:     sha256.New(),
		},
		SchemaVersion: expected.SchemaVersion,
		Ref:           expected.Ref,
		VersionID:     descriptor.versionID,
		SHA256:        descriptor.sha256,
		ContentType:   descriptor.contentType,
		Size:          descriptor.size,
		ETag:          descriptor.etag,
	}, nil
}

func (a *sd2S3ResultArchive) objectKey(taskIdentity, sourceIdentity string) string {
	return a.config.Prefix + "/" + taskIdentity[:2] + "/" + taskIdentity + "/" + sourceIdentity + ".video"
}

func (a *sd2S3ResultArchive) archivedResult(descriptor sd2ArchiveObjectDescriptor) SD2ArchivedResult {
	return SD2ArchivedResult{
		SchemaVersion: SD2ArchivedResultSchemaVersion,
		Ref:           "sd2-result://" + sd2ArchiveRefHost + "/" + descriptor.key,
		VersionID:     descriptor.versionID,
		ContentType:   descriptor.contentType,
		Size:          descriptor.size,
		SHA256:        descriptor.sha256,
		ETag:          descriptor.etag,
	}
}

func (a *sd2S3ResultArchive) keyFromRef(ref string) (string, error) {
	if err := ValidateSD2ArchivedResultRef(ref); err != nil {
		return "", err
	}
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Host != sd2ArchiveRefHost {
		return "", ErrInvalidSD2ArchivedResult
	}
	key := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasPrefix(key, a.config.Prefix+"/") || !validSD2ArchiveObjectKey(key) || !validSD2ArchiveKeyShape(a.config.Prefix, key) {
		return "", ErrInvalidSD2ArchivedResult
	}
	return key, nil
}

func validSD2ArchiveKeyShape(prefix, key string) bool {
	_, _, ok := sd2ArchiveIdentitiesFromKey(prefix, key)
	return ok
}

func sd2ArchiveIdentitiesFromKey(prefix, key string) (string, string, bool) {
	relative := strings.TrimPrefix(key, prefix+"/")
	parts := strings.Split(relative, "/")
	if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != sha256.Size*2 || len(parts[2]) != sha256.Size*2+len(".video") || parts[0] != parts[1][:2] || !strings.HasSuffix(parts[2], ".video") {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", "", false
	}
	if decoded, err := hex.DecodeString(parts[1]); err != nil || len(decoded) != sha256.Size {
		return "", "", false
	}
	sourceIdentity := strings.TrimSuffix(parts[2], ".video")
	decodedSource, err := hex.DecodeString(sourceIdentity)
	if err != nil || len(decodedSource) != sha256.Size {
		return "", "", false
	}
	return parts[1], sourceIdentity, true
}

func validSD2ArchiveObjectKey(key string) bool {
	if key == "" || len(key) > 480 || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func validSD2ArchivePublicTaskID(value string) bool {
	if value == "" || len(value) > 191 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func sd2ArchiveSourceIdentity(identityHMACKey []byte, sourceURL string) (string, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid SD2 provider result URL")
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := "https://" + strings.ToLower(parsed.Hostname()) + path
	return common.GenerateHMACWithKey(identityHMACKey, "sd2-archive-source-v1\x00"+canonical), nil
}

func (a *sd2S3ResultArchive) downloadSource(ctx context.Context, sourceURL string, allowedHosts []string) (*sd2ArchiveDownload, error) {
	if err := validateSD2ArchiveSource(sourceURL, allowedHosts); err != nil {
		return nil, err
	}
	if err := a.cleanupStaleTempFiles(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create SD2 result source request")
	}
	request.Header.Set("Accept", "video/mp4, video/webm, video/quicktime, video/x-msvideo, video/mpeg, application/octet-stream")
	request.Header.Set("Accept-Encoding", "identity")

	response, err := a.sourceClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("fetch SD2 result source")
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("fetch SD2 result source returned an invalid response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch SD2 result source returned a non-success status")
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, fmt.Errorf("fetch SD2 result source returned an encoded body")
	}
	sizeLimit := a.archiveVideoSizeLimit()
	if response.ContentLength > sizeLimit {
		return nil, fmt.Errorf("SD2 result source exceeds the archive size limit")
	}
	file, err := os.CreateTemp(a.tempDir, "sd2-result-*.video")
	if err != nil {
		return nil, fmt.Errorf("create SD2 result archive buffer")
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("secure SD2 result archive buffer")
	}

	digest := sha256.New()
	sniff := &sd2PrefixWriter{limit: 512}
	limited := &io.LimitedReader{R: response.Body, N: sizeLimit + 1}
	size, err := io.Copy(io.MultiWriter(file, digest, sniff), limited)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("read SD2 result source")
	}
	if size <= 0 || size > sizeLimit {
		cleanup()
		return nil, fmt.Errorf("SD2 result source has an invalid size")
	}
	if response.ContentLength >= 0 && response.ContentLength != size {
		cleanup()
		return nil, fmt.Errorf("SD2 result source length does not match its body")
	}
	contentType, err := canonicalSD2VideoContentType(response.Header.Get("Content-Type"), sniff.bytes)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("flush SD2 result archive buffer")
	}

	return &sd2ArchiveDownload{
		file:        file,
		contentType: contentType,
		size:        size,
		sha256:      hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

type sd2PrefixWriter struct {
	bytes []byte
	limit int
}

func (w *sd2PrefixWriter) Write(data []byte) (int, error) {
	remaining := w.limit - len(w.bytes)
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		w.bytes = append(w.bytes, data[:remaining]...)
	}
	return len(data), nil
}

func canonicalSD2VideoContentType(headerValue string, prefix []byte) (string, error) {
	declared := ""
	if strings.TrimSpace(headerValue) != "" {
		mediaType, _, err := mime.ParseMediaType(headerValue)
		if err != nil {
			return "", fmt.Errorf("SD2 result source has an invalid content type")
		}
		declared = strings.ToLower(mediaType)
		if !isSupportedSD2SourceContentType(declared) {
			return "", fmt.Errorf("SD2 result source is not a supported video")
		}
	}

	detected := detectSD2VideoContentType(prefix)
	if detected == "" {
		return "", fmt.Errorf("SD2 result source is not a recognized video")
	}
	if declared != "" && declared != "application/octet-stream" && !compatibleSD2VideoContentTypes(declared, detected) {
		return "", fmt.Errorf("SD2 result source content type does not match its body")
	}
	return detected, nil
}

func isSupportedSD2SourceContentType(contentType string) bool {
	switch contentType {
	case "video/mp4", "video/webm", "video/quicktime", "video/x-msvideo", "video/avi", "video/mpeg", "application/octet-stream":
		return true
	default:
		return false
	}
}

func compatibleSD2VideoContentTypes(declared, detected string) bool {
	if declared == detected {
		return true
	}
	if (declared == "video/mp4" || declared == "video/quicktime") && (detected == "video/mp4" || detected == "video/quicktime") {
		return true
	}
	return (declared == "video/avi" || declared == "video/x-msvideo") && detected == "video/x-msvideo"
}

func detectSD2VideoContentType(prefix []byte) string {
	if len(prefix) >= 12 && string(prefix[4:8]) == "ftyp" {
		if string(prefix[8:12]) == "qt  " {
			return "video/quicktime"
		}
		return "video/mp4"
	}
	if len(prefix) >= 4 && prefix[0] == 0x1a && prefix[1] == 0x45 && prefix[2] == 0xdf && prefix[3] == 0xa3 {
		return "video/webm"
	}
	if len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "AVI " {
		return "video/x-msvideo"
	}
	if len(prefix) >= 4 && prefix[0] == 0x00 && prefix[1] == 0x00 && prefix[2] == 0x01 && (prefix[3] == 0xba || prefix[3] == 0xb3) {
		return "video/mpeg"
	}
	return ""
}

func (a *sd2S3ResultArchive) headVerifiedObject(ctx context.Context, key, expectedTask, expectedSource, versionID string) (sd2ArchiveObjectDescriptor, bool, error) {
	input := &s3.HeadObjectInput{
		Bucket:              aws.String(a.config.Bucket),
		Key:                 aws.String(key),
		ExpectedBucketOwner: optionalAWSString(a.config.ExpectedBucketOwner),
	}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	output, err := a.client.HeadObject(ctx, input)
	if err != nil {
		if isSD2S3NotFound(err) {
			return sd2ArchiveObjectDescriptor{}, false, nil
		}
		return sd2ArchiveObjectDescriptor{}, false, fmt.Errorf("inspect SD2 result object")
	}
	if output == nil {
		return sd2ArchiveObjectDescriptor{}, false, ErrInvalidSD2ArchivedResult
	}
	descriptor, err := a.validateHeadObject(key, output, expectedTask, expectedSource)
	if err != nil {
		return sd2ArchiveObjectDescriptor{}, false, err
	}
	return descriptor, true, nil
}

func (a *sd2S3ResultArchive) validateHeadObject(key string, output *s3.HeadObjectOutput, expectedTask, expectedSource string) (sd2ArchiveObjectDescriptor, error) {
	keyTask, keySource, validKey := sd2ArchiveIdentitiesFromKey(a.config.Prefix, key)
	if !validKey {
		return sd2ArchiveObjectDescriptor{}, ErrInvalidSD2ArchivedResult
	}
	metadata := normalizeSD2ArchiveMetadata(output.Metadata)
	contentType := normalizedSD2ContentType(aws.ToString(output.ContentType))
	size := aws.ToInt64(output.ContentLength)
	checksum := strings.ToLower(strings.TrimSpace(metadata[sd2ArchiveMetadataSHA256]))
	etag := strings.TrimSpace(aws.ToString(output.ETag))
	versionID := strings.TrimSpace(aws.ToString(output.VersionId))
	metadataSize, sizeErr := strconv.ParseInt(metadata[sd2ArchiveMetadataSize], 10, 64)
	decodedChecksum, checksumErr := hex.DecodeString(checksum)
	retentionDays, retentionErr := strconv.Atoi(metadata[sd2ArchiveMetadataRetention])

	if metadata[sd2ArchiveMetadataSchema] != sd2ArchiveObjectSchemaVersion ||
		metadata[sd2ArchiveMetadataTask] != keyTask || metadata[sd2ArchiveMetadataSource] != keySource ||
		(expectedTask != "" && metadata[sd2ArchiveMetadataTask] != expectedTask) ||
		(expectedSource != "" && metadata[sd2ArchiveMetadataSource] != expectedSource) ||
		contentType == "" || metadata[sd2ArchiveMetadataContentType] != contentType || !isSupportedSD2StoredContentType(contentType) ||
		size <= 0 || size > MaxSD2ArchivedVideoBytes || sizeErr != nil || metadataSize != size || metadata[sd2ArchiveMetadataSize] != strconv.FormatInt(size, 10) ||
		checksumErr != nil || len(decodedChecksum) != sha256.Size ||
		metadata[sd2ArchiveMetadataSSE] != string(a.config.SSE) || output.ServerSideEncryption != a.config.SSE ||
		retentionErr != nil || retentionDays != a.config.RetentionDays ||
		strings.TrimSpace(aws.ToString(output.CacheControl)) != sd2ArchiveCacheControl ||
		!validSD2ArchiveOpaqueValue(etag, 512) || !validSD2ArchiveOpaqueValue(versionID, 1024) ||
		strings.EqualFold(versionID, "null") {
		return sd2ArchiveObjectDescriptor{}, ErrInvalidSD2ArchivedResult
	}
	if a.config.SSE == types.ServerSideEncryptionAwsKms && !matchesSD2ArchiveKMSKey(a.config.KMSKeyID, aws.ToString(output.SSEKMSKeyId)) {
		return sd2ArchiveObjectDescriptor{}, ErrInvalidSD2ArchivedResult
	}
	return sd2ArchiveObjectDescriptor{
		key:         key,
		versionID:   versionID,
		contentType: contentType,
		size:        size,
		sha256:      checksum,
		etag:        etag,
	}, nil
}

func (a *sd2S3ResultArchive) validateGetObject(output *s3.GetObjectOutput, expected sd2ArchiveObjectDescriptor) error {
	metadata := normalizeSD2ArchiveMetadata(output.Metadata)
	keyTask, keySource, validKey := sd2ArchiveIdentitiesFromKey(a.config.Prefix, expected.key)
	retentionDays, retentionErr := strconv.Atoi(metadata[sd2ArchiveMetadataRetention])
	if aws.ToInt64(output.ContentLength) != expected.size ||
		strings.TrimSpace(aws.ToString(output.VersionId)) != expected.versionID ||
		normalizedSD2ContentType(aws.ToString(output.ContentType)) != expected.contentType ||
		strings.TrimSpace(aws.ToString(output.CacheControl)) != sd2ArchiveCacheControl ||
		strings.TrimSpace(aws.ToString(output.ETag)) != expected.etag ||
		metadata[sd2ArchiveMetadataSHA256] != expected.sha256 ||
		metadata[sd2ArchiveMetadataSize] != strconv.FormatInt(expected.size, 10) ||
		metadata[sd2ArchiveMetadataContentType] != expected.contentType ||
		metadata[sd2ArchiveMetadataSchema] != sd2ArchiveObjectSchemaVersion ||
		!validKey || metadata[sd2ArchiveMetadataTask] != keyTask || metadata[sd2ArchiveMetadataSource] != keySource ||
		metadata[sd2ArchiveMetadataSSE] != string(a.config.SSE) ||
		retentionErr != nil || retentionDays != a.config.RetentionDays ||
		output.ServerSideEncryption != a.config.SSE ||
		(a.config.SSE == types.ServerSideEncryptionAwsKms && !matchesSD2ArchiveKMSKey(a.config.KMSKeyID, aws.ToString(output.SSEKMSKeyId))) {
		return ErrInvalidSD2ArchivedResult
	}
	return nil
}

func normalizeSD2ArchiveMetadata(metadata map[string]string) map[string]string {
	normalized := make(map[string]string, len(metadata))
	for key, value := range metadata {
		normalized[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return normalized
}

func normalizedSD2ContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func isSupportedSD2StoredContentType(contentType string) bool {
	switch contentType {
	case "video/mp4", "video/webm", "video/quicktime", "video/x-msvideo", "video/mpeg":
		return true
	default:
		return false
	}
}

func matchesSD2ArchiveKMSKey(configured, actual string) bool {
	configured = strings.TrimSpace(configured)
	actual = strings.TrimSpace(actual)
	if configured == "" || actual == "" {
		return false
	}
	if configured == actual {
		return true
	}
	return !strings.ContainsAny(configured, ":/") && strings.HasSuffix(actual, "/"+configured)
}

func optionalAWSString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return aws.String(value)
}

func isSD2S3NotFound(err error) bool {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchObject":
			return true
		}
	}
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound
}

type sd2VerifiedReadCloser struct {
	body        io.ReadCloser
	expected    int64
	read        int64
	checksum    string
	hash        hash.Hash
	done        bool
	terminalErr error
}

func (r *sd2VerifiedReadCloser) Read(buffer []byte) (int, error) {
	if r.done {
		if r.terminalErr != nil {
			err := r.terminalErr
			r.terminalErr = nil
			return 0, err
		}
		return 0, io.EOF
	}
	if r.read >= r.expected {
		var probe [1]byte
		n, err := r.body.Read(probe[:])
		if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			r.done = true
			return 0, ErrInvalidSD2ArchivedResult
		}
		r.done = true
		if hex.EncodeToString(r.hash.Sum(nil)) != r.checksum {
			return 0, ErrInvalidSD2ArchivedResult
		}
		return 0, io.EOF
	}
	remaining := r.expected - r.read
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	n, err := r.body.Read(buffer)
	if n > 0 {
		_, _ = r.hash.Write(buffer[:n])
		r.read += int64(n)
		if r.read == r.expected && hex.EncodeToString(r.hash.Sum(nil)) != r.checksum {
			r.done = true
			r.terminalErr = ErrInvalidSD2ArchivedResult
			return n, ErrInvalidSD2ArchivedResult
		}
	}
	if errors.Is(err, io.EOF) {
		r.done = true
		if r.read != r.expected || hex.EncodeToString(r.hash.Sum(nil)) != r.checksum {
			return n, ErrInvalidSD2ArchivedResult
		}
	}
	return n, err
}

func (r *sd2VerifiedReadCloser) Close() error {
	return r.body.Close()
}
