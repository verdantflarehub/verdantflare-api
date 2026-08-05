package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newVideoProxyTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	return ctx, recorder
}

func TestWritePrivateVideoResponseUsesPrivateHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newVideoProxyTestContext()

	err := writePrivateVideoResponse(ctx, strings.NewReader("video-bytes"), "video/mp4; charset=binary", 11)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "video/mp4", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "11", recorder.Header().Get("Content-Length"))
	assert.Empty(t, recorder.Header().Get("Set-Cookie"))
	assert.Equal(t, "video-bytes", recorder.Body.String())
}

func TestWritePrivateVideoResponseRejectsUnsafeMetadataBeforeWriting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("non video content type", func(t *testing.T) {
		ctx, recorder := newVideoProxyTestContext()
		err := writePrivateVideoResponse(ctx, strings.NewReader("html"), "text/html", 4)
		require.Error(t, err)
		assert.False(t, ctx.Writer.Written())
		assert.Empty(t, recorder.Body.String())
	})

	t.Run("oversized response", func(t *testing.T) {
		ctx, recorder := newVideoProxyTestContext()
		err := writePrivateVideoResponse(ctx, strings.NewReader("x"), "video/mp4", service.MaxSD2ArchivedVideoBytes+1)
		require.Error(t, err)
		assert.False(t, ctx.Writer.Written())
		assert.Empty(t, recorder.Body.String())
	})
}

func TestWriteVideoDataURLRejectsNonVideoContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newVideoProxyTestContext()

	err := writeVideoDataURL(ctx, "data:text/html;base64,PGgxPm5vdCB2aWRlbzwvaDE+")

	require.Error(t, err)
	assert.False(t, ctx.Writer.Written())
	assert.Empty(t, recorder.Body.String())
}

type videoProxyArchiveStub struct {
	openCalls int
	object    *service.SD2ArchivedObject
	expected  service.SD2ArchivedResult
}

func (a *videoProxyArchiveStub) Archive(context.Context, service.SD2ArchiveRequest) (service.SD2ArchivedResult, error) {
	return service.SD2ArchivedResult{}, service.ErrSD2ResultArchiveUnavailable
}

func (a *videoProxyArchiveStub) Open(_ context.Context, expected service.SD2ArchivedResult) (*service.SD2ArchivedObject, error) {
	a.openCalls++
	a.expected = expected
	return a.object, nil
}

func videoProxyTestDescriptor(body string) service.SD2ArchivedResult {
	digest := sha256.Sum256([]byte(body))
	return service.SD2ArchivedResult{
		SchemaVersion: service.SD2ArchivedResultSchemaVersion,
		Ref:           "sd2-result://private-results/task-public.mp4",
		VersionID:     "version-1",
		SHA256:        hex.EncodeToString(digest[:]),
		Size:          int64(len(body)),
		ContentType:   "video/mp4",
		ETag:          `"etag-1"`,
	}
}

func archivedObjectForTest(descriptor service.SD2ArchivedResult, body string) *service.SD2ArchivedObject {
	return &service.SD2ArchivedObject{
		Body:          io.NopCloser(strings.NewReader(body)),
		SchemaVersion: descriptor.SchemaVersion,
		Ref:           descriptor.Ref,
		VersionID:     descriptor.VersionID,
		SHA256:        descriptor.SHA256,
		ContentType:   descriptor.ContentType,
		Size:          descriptor.Size,
		ETag:          descriptor.ETag,
	}
}

func setupVideoProxyLedgerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.TaskSubmission{}))
	t.Cleanup(func() {
		service.SetSD2ResultArchive(nil)
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func createArchivedVideoProxyTask(t *testing.T, db *gorm.DB, userID int) *model.Task {
	t.Helper()
	snapshot := service.NewSD2TaskResultSnapshot()
	snapshot.ProviderStatus = "succeeded"
	snapshot.ArchiveState = service.SD2ArchiveStateArchived
	descriptor := videoProxyTestDescriptor("video")
	snapshot.ArchivedResult = &descriptor
	data, err := service.EncodeSD2TaskResultSnapshot(snapshot)
	require.NoError(t, err)
	task := &model.Task{
		TaskID:   "task_public",
		UserId:   userID,
		Status:   model.TaskStatusSuccess,
		Progress: "100%",
		Data:     data,
	}
	require.NoError(t, db.Create(task).Error)
	return task
}

func runVideoProxyForTest(taskID string, userID int) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+taskID+"/content", nil)
	ctx.Params = gin.Params{{Key: "task_id", Value: taskID}}
	ctx.Set("id", userID)
	VideoProxy(ctx)
	return ctx, recorder
}

func TestVideoProxyLedgerResultRequiresOwnerAndCompleteArchiveLedger(t *testing.T) {
	t.Run("cross user is hidden", func(t *testing.T) {
		db := setupVideoProxyLedgerTestDB(t)
		createArchivedVideoProxyTask(t, db, 701)

		_, recorder := runVideoProxyForTest("task_public", 702)

		assert.Equal(t, http.StatusNotFound, recorder.Code)
	})

	t.Run("orphan snapshot fails closed", func(t *testing.T) {
		db := setupVideoProxyLedgerTestDB(t)
		createArchivedVideoProxyTask(t, db, 703)
		descriptor := videoProxyTestDescriptor("video")
		archive := &videoProxyArchiveStub{object: archivedObjectForTest(descriptor, "video")}
		service.SetSD2ResultArchive(archive)

		_, recorder := runVideoProxyForTest("task_public", 703)

		assert.Equal(t, http.StatusConflict, recorder.Code)
		assert.Zero(t, archive.openCalls)
	})

	t.Run("settled archived ledger streams private object", func(t *testing.T) {
		db := setupVideoProxyLedgerTestDB(t)
		task := createArchivedVideoProxyTask(t, db, 704)
		descriptor := videoProxyTestDescriptor("video")
		taskDBID := task.ID
		require.NoError(t, db.Create(&model.TaskSubmission{
			UserID:                    704,
			PublicTaskID:              task.TaskID,
			TaskDBID:                  &taskDBID,
			PollState:                 model.TaskSubmissionPollStateTerminal,
			BillingState:              model.TaskSubmissionBillingStateSettled,
			ArchiveState:              model.TaskSubmissionArchiveStateArchived,
			ArchiveSchemaVersion:      descriptor.SchemaVersion,
			ArchivedResultRef:         descriptor.Ref,
			ArchivedResultVersionID:   descriptor.VersionID,
			ArchivedResultSHA256:      descriptor.SHA256,
			ArchivedResultSize:        descriptor.Size,
			ArchivedResultContentType: descriptor.ContentType,
			ArchivedResultETag:        descriptor.ETag,
		}).Error)
		archive := &videoProxyArchiveStub{object: archivedObjectForTest(descriptor, "video")}
		service.SetSD2ResultArchive(archive)

		_, recorder := runVideoProxyForTest("task_public", 704)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "video", recorder.Body.String())
		assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
		assert.Equal(t, 1, archive.openCalls)
		assert.Equal(t, descriptor, archive.expected)
	})

	t.Run("body checksum mismatch fails before success headers", func(t *testing.T) {
		db := setupVideoProxyLedgerTestDB(t)
		task := createArchivedVideoProxyTask(t, db, 705)
		descriptor := videoProxyTestDescriptor("video")
		taskDBID := task.ID
		require.NoError(t, db.Create(&model.TaskSubmission{
			UserID: 705, PublicTaskID: task.TaskID, TaskDBID: &taskDBID,
			PollState: model.TaskSubmissionPollStateTerminal, BillingState: model.TaskSubmissionBillingStateSettled,
			ArchiveState: model.TaskSubmissionArchiveStateArchived, ArchiveSchemaVersion: descriptor.SchemaVersion,
			ArchivedResultRef: descriptor.Ref, ArchivedResultVersionID: descriptor.VersionID,
			ArchivedResultSHA256: descriptor.SHA256, ArchivedResultSize: descriptor.Size,
			ArchivedResultContentType: descriptor.ContentType, ArchivedResultETag: descriptor.ETag,
		}).Error)
		archive := &videoProxyArchiveStub{object: archivedObjectForTest(descriptor, "bad!!")}
		service.SetSD2ResultArchive(archive)

		_, recorder := runVideoProxyForTest("task_public", 705)

		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.NotEqual(t, "video/mp4", recorder.Header().Get("Content-Type"))
	})
}
