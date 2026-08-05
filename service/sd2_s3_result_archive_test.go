package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSD2S3ArchiveConfigFailsClosed(t *testing.T) {
	config, err := loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(nil))
	require.NoError(t, err)
	assert.Nil(t, config)

	config, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(map[string]string{
		sd2ArchiveEnvEndpoint: "https://archive.example",
	}))
	require.Error(t, err)
	assert.Nil(t, config)
	assert.Contains(t, err.Error(), sd2ArchiveEnvRegion)

	values := validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvEndpoint] = "http://archive.example"
	_, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
	require.Error(t, err)

	values = validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvPrivateBucketVerified] = "false"
	_, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
	require.Error(t, err)
	assert.Contains(t, err.Error(), sd2ArchiveEnvPrivateBucketVerified)

	values = validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvVersioningVerified] = "false"
	_, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
	require.Error(t, err)
	assert.Contains(t, err.Error(), sd2ArchiveEnvVersioningVerified)

	values = validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvLifecycleVerified] = "false"
	_, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
	require.Error(t, err)
	assert.Contains(t, err.Error(), sd2ArchiveEnvLifecycleVerified)

	values = validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvSecretAccessKey] = "do-not-print-this-secret"
	delete(values, sd2ArchiveEnvSSE)
	_, err = loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "do-not-print-this-secret")

	for name, mutate := range map[string]func(map[string]string){
		"non-DNS bucket":       func(values map[string]string) { values[sd2ArchiveEnvBucket] = "Bad_Bucket" },
		"empty prefix segment": func(values map[string]string) { values[sd2ArchiveEnvPrefix] = "a//b" },
		"dot prefix segment":   func(values map[string]string) { values[sd2ArchiveEnvPrefix] = "a/./b" },
		"non-canonical channel": func(values map[string]string) {
			values[sd2ArchiveEnvHostAllowlist] = `{"wxmaas-seedance":{"019":["result.example"]}}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			values := validSD2ArchiveEnvironment()
			mutate(values)
			_, err := loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(values))
			require.Error(t, err)
		})
	}
}

func TestLoadSD2S3ArchiveConfigBuildsExactProviderChannelAllowlist(t *testing.T) {
	config, err := loadSD2S3ArchiveConfig(mapSD2ArchiveEnv(validSD2ArchiveEnvironment()))
	require.NoError(t, err)
	require.NotNil(t, config)
	assert.Equal(t, "https://archive.example", config.Endpoint)
	assert.Equal(t, "sd2-results", config.Prefix)
	assert.True(t, config.UsePathStyle)
	assert.Equal(t, types.ServerSideEncryptionAes256, config.SSE)
	clientOptions := newSD2S3Client(*config).Options()
	assert.Equal(t, aws.RequestChecksumCalculationWhenRequired, clientOptions.RequestChecksumCalculation)
	assert.Equal(t, aws.ResponseChecksumValidationWhenRequired, clientOptions.ResponseChecksumValidation)

	resolver := config.resultHostResolver()
	assert.Equal(t, []string{"result.example", "media.example"}, resolver(&model.TaskSubmission{
		Provider:  "WXMAAS-SEEDANCE",
		ChannelID: 19,
	}))
	assert.Nil(t, resolver(&model.TaskSubmission{Provider: "wxmaas-seedance", ChannelID: 20}))
	assert.Nil(t, resolver(&model.TaskSubmission{Provider: "jd-seedance", ChannelID: 19}))
}

func TestConfigureSD2ResultArchiveFromEnvRegistersWithoutConnecting(t *testing.T) {
	values := validSD2ArchiveEnvironment()
	values[sd2ArchiveEnvSessionToken] = ""
	values[sd2ArchiveEnvPrefix] = "sd2-results"
	values[sd2ArchiveEnvUsePathStyle] = "true"
	values[sd2ArchiveEnvExpectedBucketOwner] = ""
	values[sd2ArchiveEnvKMSKeyID] = ""
	values[sd2ArchiveEnvTempDir] = t.TempDir()
	for _, key := range sd2ArchiveEnvironmentKeys {
		t.Setenv(key, values[key])
	}
	t.Cleanup(func() {
		SetSD2ResultArchive(nil)
		SetSD2ResultHostAllowlistResolver(nil)
	})

	require.NoError(t, ConfigureSD2ResultArchiveFromEnv())
	assert.True(t, IsSD2ResultArchiveConfigured())
	assert.Equal(t, []string{"result.example", "media.example"}, SD2ResultHostsForSubmission(&model.TaskSubmission{
		Provider:  "wxmaas-seedance",
		ChannelID: 19,
	}))
}

func TestPrepareSD2ArchiveTempDirOwnsAndCleansOnlyStaleArchiveFiles(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	tempDir := t.TempDir()
	prepared, err := prepareSD2ArchiveTempDir(tempDir, now)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(tempDir), filepath.Clean(prepared))
	require.FileExists(t, filepath.Join(prepared, sd2ArchiveTempMarkerName))

	stalePath := filepath.Join(prepared, "sd2-result-stale.video")
	recentPath := filepath.Join(prepared, "sd2-result-recent.video")
	unrelatedPath := filepath.Join(prepared, "unrelated.video")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(recentPath, []byte("recent"), 0o600))
	require.NoError(t, os.WriteFile(unrelatedPath, []byte("unrelated"), 0o600))
	require.NoError(t, os.Chtimes(stalePath, now.Add(-sd2ArchiveStaleTempAge-time.Minute), now.Add(-sd2ArchiveStaleTempAge-time.Minute)))
	require.NoError(t, os.Chtimes(recentPath, now.Add(-time.Minute), now.Add(-time.Minute)))

	require.NoError(t, cleanupSD2ArchiveStaleTempFiles(prepared, now))
	assert.NoFileExists(t, stalePath)
	assert.FileExists(t, recentPath)
	assert.FileExists(t, unrelatedPath)
}

func TestPrepareSD2ArchiveTempDirRejectsUnownedNonEmptyDirectory(t *testing.T) {
	tempDir := t.TempDir()
	unrelatedPath := filepath.Join(tempDir, "do-not-delete.txt")
	require.NoError(t, os.WriteFile(unrelatedPath, []byte("keep"), 0o600))

	_, err := prepareSD2ArchiveTempDir(tempDir, time.Now())
	require.Error(t, err)
	assert.FileExists(t, unrelatedPath)
}

func TestSD2ArchiveSourceClientPinsDNSAndRejectsPrivateAddresses(t *testing.T) {
	disableSSRFForSD2ArchiveTest(t)
	resolver := staticSD2ArchiveResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	dialCalls := 0
	client, err := newSD2ArchiveSourceHTTPClient(resolver, func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("must not dial")
	})
	require.NoError(t, err)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, transport.Proxy)
	require.NotNil(t, transport.TLSClientConfig)
	assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://result.example/video.mp4", nil)
	require.NoError(t, err)
	_, err = client.Do(request)
	require.Error(t, err)
	assert.Zero(t, dialCalls)
	assert.Contains(t, err.Error(), "private IP")
}

func TestSD2ArchiveSourceClientDoesNotFollowRedirects(t *testing.T) {
	client, err := newSD2ArchiveSourceHTTPClient(nil, nil)
	require.NoError(t, err)
	calls := 0
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://other.example/video.mp4"}},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    request,
		}, nil
	})

	response, err := client.Get("https://result.example/video.mp4")
	require.NoError(t, err)
	require.NotNil(t, response)
	defer response.Body.Close()
	assert.Equal(t, http.StatusFound, response.StatusCode)
	assert.Equal(t, 1, calls)
}

func TestSD2S3ResultArchiveIdempotentPutHeadAndOpen(t *testing.T) {
	storage := newMemorySD2S3()
	archive := newTestSD2S3Archive(t, storage)
	video := testMP4Bytes()
	fetches := 0
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		fetches++
		assert.Empty(t, request.Header.Get("Authorization"))
		assert.Empty(t, request.Header.Get("Cookie"))
		assert.Equal(t, "identity", request.Header.Get("Accept-Encoding"))
		return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
	})

	request := SD2ArchiveRequest{
		PublicTaskID:       "task_public_1",
		UserID:             7,
		SourceURL:          "https://result.example/video.mp4?signature=first-secret",
		AllowedSourceHosts: []string{"result.example"},
	}
	first, err := archive.Archive(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, SD2ArchivedResultSchemaVersion, first.SchemaVersion)
	assert.Equal(t, "video/mp4", first.ContentType)
	assert.Equal(t, int64(len(video)), first.Size)
	assert.NotEmpty(t, first.VersionID)
	assert.NotEmpty(t, first.ETag)
	assert.Len(t, first.SHA256, sha256.Size*2)
	assert.NotContains(t, first.Ref, "task_public_1")
	assert.NotContains(t, first.Ref, "result.example")
	assert.NotContains(t, first.Ref, "first-secret")
	require.NoError(t, ValidateSD2ArchivedResultRef(first.Ref))

	storage.mu.Lock()
	require.NotNil(t, storage.lastPut)
	assert.Equal(t, "*", aws.ToString(storage.lastPut.IfNoneMatch))
	assert.Empty(t, storage.lastPut.ACL)
	assert.Equal(t, types.ServerSideEncryptionAes256, storage.lastPut.ServerSideEncryption)
	assert.Equal(t, sd2ArchiveCacheControl, aws.ToString(storage.lastPut.CacheControl))
	storage.mu.Unlock()

	request.SourceURL = "https://result.example/video.mp4?signature=refreshed-secret"
	second, err := archive.Archive(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, fetches)
	assert.Equal(t, 1, storage.putCount())

	object, err := archive.Open(context.Background(), first)
	require.NoError(t, err)
	require.NotNil(t, object)
	opened, err := io.ReadAll(object.Body)
	require.NoError(t, err)
	require.NoError(t, object.Body.Close())
	assert.Equal(t, video, opened)
	assert.Equal(t, int64(len(video)), object.Size)
	assert.Equal(t, "video/mp4", object.ContentType)
	assert.Equal(t, 1, storage.getCount())
	storage.mu.Lock()
	require.NotNil(t, storage.lastHead)
	require.NotNil(t, storage.lastGet)
	assert.Equal(t, first.VersionID, aws.ToString(storage.lastHead.VersionId))
	assert.Equal(t, first.VersionID, aws.ToString(storage.lastGet.VersionId))
	assert.Equal(t, first.ETag, aws.ToString(storage.lastGet.IfMatch))
	storage.mu.Unlock()
}

func TestSD2S3ResultArchiveRequiresVersionIDAndChecksum(t *testing.T) {
	t.Run("missing version id", func(t *testing.T) {
		storage := newMemorySD2S3()
		storage.omitVersionID = true
		archive := newTestSD2S3Archive(t, storage)
		video := testMP4Bytes()
		archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
		})

		_, err := archive.Archive(context.Background(), SD2ArchiveRequest{
			PublicTaskID: "task_missing_version", UserID: 20,
			SourceURL: "https://result.example/video.mp4", AllowedSourceHosts: []string{"result.example"},
		})
		require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
	})

	t.Run("literal null version id", func(t *testing.T) {
		storage := newMemorySD2S3()
		storage.nullVersionID = true
		archive := newTestSD2S3Archive(t, storage)
		video := testMP4Bytes()
		archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
		})

		_, err := archive.Archive(context.Background(), SD2ArchiveRequest{
			PublicTaskID: "task_null_version", UserID: 23,
			SourceURL: "https://result.example/video.mp4", AllowedSourceHosts: []string{"result.example"},
		})
		require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
	})

	t.Run("missing checksum", func(t *testing.T) {
		storage := newMemorySD2S3()
		archive := newTestSD2S3Archive(t, storage)
		video := testMP4Bytes()
		archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
		})
		result, err := archive.Archive(context.Background(), SD2ArchiveRequest{
			PublicTaskID: "task_missing_checksum", UserID: 21,
			SourceURL: "https://result.example/video.mp4", AllowedSourceHosts: []string{"result.example"},
		})
		require.NoError(t, err)
		storage.mu.Lock()
		for _, object := range storage.objects {
			delete(object.metadata, sd2ArchiveMetadataSHA256)
		}
		storage.mu.Unlock()

		_, err = archive.Open(context.Background(), result)
		require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
	})
}

func TestSD2S3ResultArchiveFrozenDescriptorRejectsReplacementAndETagMismatch(t *testing.T) {
	storage := newMemorySD2S3()
	archive := newTestSD2S3Archive(t, storage)
	video := testMP4Bytes()
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
	})
	result, err := archive.Archive(context.Background(), SD2ArchiveRequest{
		PublicTaskID: "task_replacement", UserID: 22,
		SourceURL: "https://result.example/video.mp4", AllowedSourceHosts: []string{"result.example"},
	})
	require.NoError(t, err)

	etagMismatch := result
	etagMismatch.ETag = `"different-etag"`
	_, err = archive.Open(context.Background(), etagMismatch)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)

	storage.mu.Lock()
	for _, object := range storage.objects {
		object.versionID = "replacement-version"
		object.etag = `"replacement-etag"`
		object.body = append([]byte(nil), video...)
	}
	storage.mu.Unlock()
	_, err = archive.Open(context.Background(), result)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
}

func TestSD2S3ResultArchiveReusesCommittedObjectAfterAmbiguousPutFailure(t *testing.T) {
	storage := newMemorySD2S3()
	storage.commitThenError = true
	archive := newTestSD2S3Archive(t, storage)
	video := testMP4Bytes()
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "application/octet-stream", video, int64(len(video))), nil
	})

	result, err := archive.Archive(context.Background(), SD2ArchiveRequest{
		PublicTaskID:       "task_orphan_reuse",
		UserID:             9,
		SourceURL:          "https://result.example/video.mp4?signature=secret",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Ref)
	assert.Equal(t, 1, storage.putCount())
}

func TestSD2S3ResultArchiveRejectsOversizeAndInvalidMIME(t *testing.T) {
	storage := newMemorySD2S3()
	archive := newTestSD2S3Archive(t, storage)
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", []byte("unused"), MaxSD2ArchivedVideoBytes+1), nil
	})

	_, err := archive.downloadSource(context.Background(), "https://result.example/video.mp4", []string{"result.example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size limit")

	archive.maxVideoSize = 8
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", testMP4Bytes(), -1), nil
	})
	_, err = archive.downloadSource(context.Background(), "https://result.example/video.mp4", []string{"result.example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid size")
	archive.maxVideoSize = MaxSD2ArchivedVideoBytes

	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "text/html", []byte("<html>not a video</html>"), int64(len("<html>not a video</html>"))), nil
	})
	_, err = archive.downloadSource(context.Background(), "https://result.example/video.mp4", []string{"result.example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "supported video")

	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", []byte("not an mp4"), int64(len("not an mp4"))), nil
	})
	_, err = archive.downloadSource(context.Background(), "https://result.example/video.mp4", []string{"result.example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recognized video")
}

func TestSD2S3ResultArchiveRejectsTamperedObjectMetadata(t *testing.T) {
	storage := newMemorySD2S3()
	archive := newTestSD2S3Archive(t, storage)
	video := testMP4Bytes()
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
	})
	result, err := archive.Archive(context.Background(), SD2ArchiveRequest{
		PublicTaskID:       "task_metadata",
		UserID:             11,
		SourceURL:          "https://result.example/video.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.NoError(t, err)

	storage.mu.Lock()
	for _, object := range storage.objects {
		object.metadata[sd2ArchiveMetadataSHA256] = "invalid"
	}
	storage.mu.Unlock()

	_, err = archive.Open(context.Background(), result)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
}

func TestSD2S3ResultArchiveDetectsBodyChecksumMismatchOnOpen(t *testing.T) {
	storage := newMemorySD2S3()
	archive := newTestSD2S3Archive(t, storage)
	video := testMP4Bytes()
	archive.sourceClient = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sourceResponse(request, http.StatusOK, "video/mp4", video, int64(len(video))), nil
	})
	result, err := archive.Archive(context.Background(), SD2ArchiveRequest{
		PublicTaskID:       "task_checksum",
		UserID:             12,
		SourceURL:          "https://result.example/checksum.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.NoError(t, err)

	storage.mu.Lock()
	for _, object := range storage.objects {
		object.body[len(object.body)-1] ^= 0xff
	}
	storage.mu.Unlock()

	object, err := archive.Open(context.Background(), result)
	require.NoError(t, err)
	_, err = io.ReadAll(object.Body)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
	require.NoError(t, object.Body.Close())

	object, err = archive.Open(context.Background(), result)
	require.NoError(t, err)
	written, copyErr := io.CopyN(io.Discard, object.Body, object.Size)
	assert.Equal(t, object.Size, written)
	if copyErr == nil {
		var probe [1]byte
		_, copyErr = object.Body.Read(probe[:])
	}
	require.ErrorIs(t, copyErr, ErrInvalidSD2ArchivedResult)
	require.NoError(t, object.Body.Close())
}

func TestValidateSD2ArchiveSourceUsesExactHTTPSAllowlist(t *testing.T) {
	require.NoError(t, validateSD2ArchiveSource("https://result.example/video.mp4?signature=secret", []string{"RESULT.EXAMPLE"}))
	require.Error(t, validateSD2ArchiveSource("https://evil.result.example/video.mp4", []string{"result.example"}))
	require.Error(t, validateSD2ArchiveSource("https://result.example.evil/video.mp4", []string{"result.example"}))
	require.Error(t, validateSD2ArchiveSource("https://result.example:8443/video.mp4", []string{"result.example"}))
	require.Error(t, validateSD2ArchiveSource("https://result.example:/video.mp4", []string{"result.example"}))
	require.Error(t, validateSD2ArchiveSource("http://result.example/video.mp4", []string{"result.example"}))
}

func validSD2ArchiveEnvironment() map[string]string {
	return map[string]string{
		sd2ArchiveEnvEndpoint:              "https://archive.example",
		sd2ArchiveEnvRegion:                "cn-test-1",
		sd2ArchiveEnvBucket:                "private-results",
		sd2ArchiveEnvAccessKeyID:           "test-access-key",
		sd2ArchiveEnvSecretAccessKey:       "test-secret-key",
		sd2ArchiveEnvSSE:                   "AES256",
		sd2ArchiveEnvIdentityHMACKey:       "stable-test-identity-hmac-key-32-bytes-minimum",
		sd2ArchiveEnvTempDir:               filepath.Join(os.TempDir(), "new-api-sd2-result-archive-config-test"),
		sd2ArchiveEnvPrivateBucketVerified: "true",
		sd2ArchiveEnvVersioningVerified:    "true",
		sd2ArchiveEnvLifecycleVerified:     "true",
		sd2ArchiveEnvRetentionDays:         "30",
		sd2ArchiveEnvHostAllowlist:         `{"wxmaas-seedance":{"19":["result.example","media.example","result.example"]}}`,
	}
}

func mapSD2ArchiveEnv(values map[string]string) sd2ArchiveEnvLookup {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

type staticSD2ArchiveResolver struct {
	addresses []net.IPAddr
	err       error
}

func (r staticSD2ArchiveResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return r.addresses, r.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (f roundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func sourceResponse(request *http.Request, status int, contentType string, body []byte, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: contentLength,
		Request:       request,
	}
}

func testMP4Bytes() []byte {
	return []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0x00, 0x00, 0x02, 0x00, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}
}

func newTestSD2S3Archive(t *testing.T, client sd2S3API) *sd2S3ResultArchive {
	t.Helper()
	tempDir := t.TempDir()
	archive, err := newSD2S3ResultArchive(sd2S3ArchiveConfig{
		Endpoint:        "https://archive.example",
		Region:          "cn-test-1",
		Bucket:          "private-results",
		Prefix:          "sd2-results",
		SSE:             types.ServerSideEncryptionAes256,
		IdentityHMACKey: []byte("stable-test-identity-hmac-key-32-bytes-minimum"),
		TempDir:         tempDir,
		RetentionDays:   30,
	}, client)
	require.NoError(t, err)
	return archive
}

type memorySD2S3Object struct {
	body         []byte
	contentType  string
	cacheControl string
	metadata     map[string]string
	sse          types.ServerSideEncryption
	kmsKeyID     string
	etag         string
	versionID    string
}

type memorySD2S3 struct {
	mu              sync.Mutex
	objects         map[string]*memorySD2S3Object
	puts            int
	gets            int
	lastPut         *s3.PutObjectInput
	lastHead        *s3.HeadObjectInput
	lastGet         *s3.GetObjectInput
	commitThenError bool
	omitVersionID   bool
	nullVersionID   bool
}

func newMemorySD2S3() *memorySD2S3 {
	return &memorySD2S3{objects: make(map[string]*memorySD2S3Object)}
}

func (s *memorySD2S3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHead = input
	object := s.objects[aws.ToString(input.Key)]
	if object == nil {
		return nil, &smithy.GenericAPIError{Code: "NotFound", Message: "not found"}
	}
	if requestedVersion := aws.ToString(input.VersionId); requestedVersion != "" && requestedVersion != object.versionID {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "version not found"}
	}
	output := &s3.HeadObjectOutput{
		CacheControl:         aws.String(object.cacheControl),
		ContentLength:        aws.Int64(int64(len(object.body))),
		ContentType:          aws.String(object.contentType),
		ETag:                 aws.String(object.etag),
		Metadata:             cloneStringMap(object.metadata),
		ServerSideEncryption: object.sse,
		SSEKMSKeyId:          optionalAWSString(object.kmsKeyID),
	}
	if !s.omitVersionID {
		versionID := object.versionID
		if s.nullVersionID {
			versionID = "null"
		}
		output.VersionId = aws.String(versionID)
	}
	return output, nil
}

func (s *memorySD2S3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.lastPut = input
	key := aws.ToString(input.Key)
	if _, exists := s.objects[key]; exists && aws.ToString(input.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "already exists"}
	}
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:8]) + `"`
	versionID := fmt.Sprintf("version-%d", s.puts)
	s.objects[key] = &memorySD2S3Object{
		body:         append([]byte(nil), body...),
		contentType:  aws.ToString(input.ContentType),
		cacheControl: aws.ToString(input.CacheControl),
		metadata:     cloneStringMap(input.Metadata),
		sse:          input.ServerSideEncryption,
		kmsKeyID:     aws.ToString(input.SSEKMSKeyId),
		etag:         etag,
		versionID:    versionID,
	}
	if s.commitThenError {
		return nil, errors.New("ambiguous transport failure")
	}
	return &s3.PutObjectOutput{ETag: aws.String(etag), VersionId: aws.String(versionID)}, nil
}

func (s *memorySD2S3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	s.lastGet = input
	object := s.objects[aws.ToString(input.Key)]
	if object == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "not found"}
	}
	if aws.ToString(input.VersionId) != object.versionID || aws.ToString(input.IfMatch) != object.etag {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "frozen descriptor mismatch"}
	}
	return &s3.GetObjectOutput{
		Body:                 io.NopCloser(bytes.NewReader(append([]byte(nil), object.body...))),
		CacheControl:         aws.String(object.cacheControl),
		ContentLength:        aws.Int64(int64(len(object.body))),
		ContentType:          aws.String(object.contentType),
		ETag:                 aws.String(object.etag),
		Metadata:             cloneStringMap(object.metadata),
		ServerSideEncryption: object.sse,
		SSEKMSKeyId:          optionalAWSString(object.kmsKeyID),
		VersionId:            aws.String(object.versionID),
	}, nil
}

func (s *memorySD2S3) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *memorySD2S3) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
