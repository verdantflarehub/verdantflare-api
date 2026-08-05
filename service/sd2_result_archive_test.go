package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSD2ResultArchive struct {
	archiveResult SD2ArchivedResult
	archiveErr    error
	openObject    *SD2ArchivedObject
	openErr       error
	request       SD2ArchiveRequest
}

func (a *fakeSD2ResultArchive) Archive(_ context.Context, request SD2ArchiveRequest) (SD2ArchivedResult, error) {
	a.request = request
	return a.archiveResult, a.archiveErr
}

func (a *fakeSD2ResultArchive) Open(_ context.Context, _ SD2ArchivedResult) (*SD2ArchivedObject, error) {
	return a.openObject, a.openErr
}

func validTestSD2ArchivedResult(ref string, size int64) SD2ArchivedResult {
	return SD2ArchivedResult{
		SchemaVersion: SD2ArchivedResultSchemaVersion,
		Ref:           ref,
		VersionID:     "version-1",
		SHA256:        strings.Repeat("ab", 32),
		Size:          size,
		ContentType:   "video/mp4",
		ETag:          `"etag-1"`,
	}
}

func disableSSRFForSD2ArchiveTest(t *testing.T) {
	t.Helper()
	fetchSetting := system_setting.GetFetchSetting()
	original := *fetchSetting
	t.Cleanup(func() { *fetchSetting = original })
	fetchSetting.EnableSSRFProtection = false
}

func TestSD2TaskResultSnapshotRejectsPublicResultURL(t *testing.T) {
	snapshot := NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "succeeded"
	snapshot.ArchiveState = SD2ArchiveStateArchived
	invalidDescriptor := validTestSD2ArchivedResult("https://provider.example/video.mp4?signature=secret", 1024)
	snapshot.ArchivedResult = &invalidDescriptor

	require.Error(t, snapshot.Validate())

	validDescriptor := validTestSD2ArchivedResult("sd2-result://private-results/user-7/task-public.mp4", 1024)
	snapshot.ArchivedResult = &validDescriptor
	require.NoError(t, snapshot.Validate())
	snapshot.Usage = SD2TaskUsageSnapshot{CompletionTokens: 1, TotalTokens: 0}
	require.Error(t, snapshot.Validate())
	snapshot.Usage = SD2TaskUsageSnapshot{}
	encoded, err := common.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "signature=secret")
	assert.NotContains(t, string(encoded), "video_url")
}

func TestDecodeSD2TaskResultSnapshotRejectsProviderResponseFields(t *testing.T) {
	_, err := DecodeSD2TaskResultSnapshot([]byte(`{
		"schema_version":1,
		"provider_status":"succeeded",
		"archive_state":"ARCHIVED",
		"archived_result":{
			"schema_version":1,
			"ref":"sd2-result://private-results/task.mp4",
			"version_id":"version-1",
			"sha256":"abababababababababababababababababababababababababababababababab",
			"size":1024,
			"content_type":"video/mp4",
			"etag":"etag-1"
		},
		"video_url":"https://provider.example/video.mp4?signature=secret"
	}`))

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "signature=secret")
}

func TestSD2TaskResultSnapshotRequiresCompleteFrozenDescriptor(t *testing.T) {
	descriptor := validTestSD2ArchivedResult("sd2-result://private-results/task.mp4", 1024)
	for name, mutate := range map[string]func(*SD2ArchivedResult){
		"schema version": func(value *SD2ArchivedResult) { value.SchemaVersion = 0 },
		"version id":     func(value *SD2ArchivedResult) { value.VersionID = "" },
		"null version":   func(value *SD2ArchivedResult) { value.VersionID = "null" },
		"checksum":       func(value *SD2ArchivedResult) { value.SHA256 = "" },
		"size":           func(value *SD2ArchivedResult) { value.Size = 0 },
		"content type":   func(value *SD2ArchivedResult) { value.ContentType = "video/mp4; charset=binary" },
		"etag":           func(value *SD2ArchivedResult) { value.ETag = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := descriptor
			mutate(&invalid)
			snapshot := NewSD2TaskResultSnapshot()
			snapshot.ArchiveState = SD2ArchiveStateArchived
			snapshot.ArchivedResult = &invalid
			require.ErrorIs(t, snapshot.Validate(), ErrInvalidSD2ArchivedResult)
		})
	}
}

func TestArchiveSD2ProviderResultFailsClosedWithoutPrivateArchive(t *testing.T) {
	disableSSRFForSD2ArchiveTest(t)
	SetSD2ResultArchive(nil)
	t.Cleanup(func() { SetSD2ResultArchive(nil) })

	_, err := ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		PublicTaskID:       "task_public",
		UserID:             7,
		SourceURL:          "https://result.example/video.mp4?signature=secret",
		AllowedSourceHosts: []string{"result.example"},
	})

	require.ErrorIs(t, err, ErrSD2ResultArchiveUnavailable)
}

func TestArchiveSD2ProviderResultRequiresAllowlistedHTTPSAndVerifiedObject(t *testing.T) {
	disableSSRFForSD2ArchiveTest(t)
	fakeArchive := &fakeSD2ResultArchive{
		archiveResult: validTestSD2ArchivedResult("sd2-result://private-results/user-7/task-public.mp4", 1024),
	}
	SetSD2ResultArchive(fakeArchive)
	t.Cleanup(func() { SetSD2ResultArchive(nil) })

	_, err := ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		SourceURL:          "http://result.example/video.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.Error(t, err)

	_, err = ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		SourceURL:          "https://other.example/video.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.Error(t, err)

	result, err := ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		PublicTaskID:       "task_public",
		UserID:             7,
		SourceURL:          "https://result.example/video.mp4?signature=secret",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.NoError(t, err)
	assert.Equal(t, fakeArchive.archiveResult, result)
	assert.Equal(t, "task_public", fakeArchive.request.PublicTaskID)
}

func TestOpenSD2ArchivedResultRejectsOversizedOrNonVideoObject(t *testing.T) {
	descriptor := validTestSD2ArchivedResult("sd2-result://private-results/task.mp4", 11)
	fakeArchive := &fakeSD2ResultArchive{
		openObject: &SD2ArchivedObject{
			Body:          io.NopCloser(strings.NewReader("not a video")),
			SchemaVersion: descriptor.SchemaVersion,
			Ref:           descriptor.Ref,
			VersionID:     descriptor.VersionID,
			SHA256:        descriptor.SHA256,
			ContentType:   "text/html",
			Size:          descriptor.Size,
			ETag:          descriptor.ETag,
		},
	}
	SetSD2ResultArchive(fakeArchive)
	t.Cleanup(func() { SetSD2ResultArchive(nil) })

	_, err := OpenSD2ArchivedResult(context.Background(), descriptor)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)

	oversized := descriptor
	oversized.Size = MaxSD2ArchivedVideoBytes + 1
	_, err = OpenSD2ArchivedResult(context.Background(), oversized)
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)
}

func TestArchiveSD2ProviderResultRejectsInvalidArchiveMetadata(t *testing.T) {
	disableSSRFForSD2ArchiveTest(t)
	fakeArchive := &fakeSD2ResultArchive{
		archiveResult: validTestSD2ArchivedResult("sd2-result://private-results/task.mp4", 10),
	}
	fakeArchive.archiveResult.SHA256 = "not-a-sha256"
	SetSD2ResultArchive(fakeArchive)
	t.Cleanup(func() { SetSD2ResultArchive(nil) })

	_, err := ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		SourceURL:          "https://result.example/video.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.ErrorIs(t, err, ErrInvalidSD2ArchivedResult)

	fakeArchive.archiveErr = errors.New("store unavailable")
	_, err = ArchiveSD2ProviderResult(context.Background(), SD2ArchiveRequest{
		SourceURL:          "https://result.example/video.mp4",
		AllowedSourceHosts: []string{"result.example"},
	})
	require.EqualError(t, err, "store unavailable")
}

func TestValidateSD2ResultDeliveryReadyFailsClosed(t *testing.T) {
	submission := &model.TaskSubmission{ID: 1, Provider: "wxmaas-seedance"}
	SetSD2ResultArchive(nil)
	SetSD2ResultHostAllowlistResolver(nil)
	t.Cleanup(func() {
		SetSD2ResultArchive(nil)
		SetSD2ResultHostAllowlistResolver(nil)
	})

	require.ErrorIs(t, ValidateSD2ResultDeliveryReady(submission), ErrSD2ResultArchiveUnavailable)

	SetSD2ResultArchive(&fakeSD2ResultArchive{})
	require.EqualError(t, ValidateSD2ResultDeliveryReady(submission), "sd2 provider result host allowlist is not configured")

	SetSD2ResultHostAllowlistResolver(func(*model.TaskSubmission) []string {
		return []string{" RESULT.EXAMPLE ", "result.example", "https://not-a-host.example", "result.example/path"}
	})
	assert.Equal(t, []string{"result.example"}, SD2ResultHostsForSubmission(submission))
	require.NoError(t, ValidateSD2ResultDeliveryReady(submission))
}
