package model

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestLogContentDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for _, scenario := range []string{"fresh", "upgrade_rc44"} {
				t.Run(scenario, func(t *testing.T) {
					var driver gorm.Dialector
					switch dialect {
					case "sqlite":
						driver = sqlite.Open(":memory:")
					case "mysql":
						dsn := os.Getenv("TEST_MYSQL_DSN")
						if dsn == "" {
							t.Skip("TEST_MYSQL_DSN is not configured")
						}
						driver = mysql.Open(dsn)
					case "postgres":
						dsn := os.Getenv("TEST_POSTGRES_DSN")
						if dsn == "" {
							t.Skip("TEST_POSTGRES_DSN is not configured")
						}
						driver = postgres.Open(dsn)
					}
					db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "capture_matrix_" + scenario + "_"}})
					require.NoError(t, err)
					connection, err := db.DB()
					require.NoError(t, err)
					connection.SetMaxOpenConns(1)
					previousDB, previousType, previousEnabled := LOG_DB, common.LogDatabaseType(), common.LogRequestResponseEnabled
					LOG_DB = db
					common.SetLogDatabaseType(common.DatabaseType(dialect))
					common.LogRequestResponseEnabled = true
					t.Cleanup(func() {
						require.NoError(t, db.Migrator().DropTable(&LogContentChunk{}, &Log{}))
						require.NoError(t, connection.Close())
						LOG_DB, common.LogRequestResponseEnabled = previousDB, previousEnabled
						common.SetLogDatabaseType(previousType)
					})
					legacy := releasedCaptureLog{RequestId: "legacy", CreatedAt: 1, Type: LogTypeConsume, Content: "preserved usage", Quota: 42}
					if scenario == "upgrade_rc44" {
						table := db.NamingStrategy.TableName("Log")
						require.NoError(t, db.Table(table).AutoMigrate(&releasedCaptureLog{}))
						require.NoError(t, db.Table(table).Create(&legacy).Error)
					}
					require.NoError(t, migrateLOGDB())
					require.NoError(t, migrateLOGDB())
					versionQuery := "SELECT version()"
					if dialect == "sqlite" {
						versionQuery = "SELECT sqlite_version()"
					}
					var version string
					require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
					t.Logf("separate log database version: %s", version)
					if scenario == "upgrade_rc44" {
						var saved Log
						require.NoError(t, db.First(&saved, legacy.Id).Error)
						assert.Equal(t, legacy.Content, saved.Content)
						assert.Equal(t, legacy.Quota, saved.Quota)
						assert.Equal(t, legacy.RequestId, saved.RequestId)
					}
					request := strings.Repeat("request 内容", 8000)
					response := strings.Repeat("response 内容", 8000)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(request))
					c.Set(common.RequestIdKey, "matrix-capture")
					require.NoError(t, createLog(&Log{RequestId: "matrix-capture", Type: LogTypeConsume, CreatedAt: common.GetTimestamp()}))
					common.BeginRelayContentCapture(c)
					_, err = c.Writer.WriteString(response)
					require.NoError(t, err)
					PersistRelayContentCapture(c)
					for _, body := range []struct{ kind, text string }{{common.RelayContentKindRequest, request}, {common.RelayContentKindResponse, response}} {
						page, err := GetLogContentPage("matrix-capture", body.kind, 1, 100)
						require.NoError(t, err)
						var decoded []byte
						for _, encoded := range page.Chunks {
							chunk, err := base64.StdEncoding.DecodeString(encoded)
							require.NoError(t, err)
							decoded = append(decoded, chunk...)
						}
						assert.Equal(t, body.text, string(decoded))
						assert.EqualValues(t, len(body.text), page.TotalSize)
					}
					// Repeated startup preserves captured data and its uniqueness guarantee.
					require.NoError(t, migrateLOGDB())
					require.Error(t, db.Create(&LogContentChunk{RequestId: "matrix-capture", Kind: common.RelayContentKindRequest, ChunkIndex: 0}).Error)
					var chunkCount int64
					require.NoError(t, db.Model(&LogContentChunk{}).Count(&chunkCount).Error)
					deleted, err := DeleteExpiredLogContentChunksBatch(t.Context(), common.GetTimestamp()+1, 1)
					require.NoError(t, err)
					assert.EqualValues(t, 1, deleted)
					remaining, err := CountExpiredLogContentChunks(t.Context(), common.GetTimestamp()+1)
					require.NoError(t, err)
					assert.Equal(t, chunkCount-1, remaining)
					var logCount int64
					require.NoError(t, db.Model(&Log{}).Where("request_id = ?", "matrix-capture").Count(&logCount).Error)
					assert.EqualValues(t, 1, logCount, "content retention keeps the usage log")
					_, err = DeleteOldLogBatch(t.Context(), common.GetTimestamp()+1, 100)
					require.NoError(t, err)
					require.NoError(t, db.Model(&LogContentChunk{}).Count(&chunkCount).Error)
					assert.Zero(t, chunkCount, "deleting usage logs also deletes captured content")
				})
			}
		})
	}
}

func TestDeleteExpiredLogContentChunksBatchIsBoundedAndPreservesUsageLogs(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:log-content-cleanup-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}, &LogContentChunk{}))

	previousLogDB := LOG_DB
	previousLogDatabaseType := common.LogDatabaseType()
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogDatabaseType)
	})

	require.NoError(t, db.Create(&Log{RequestId: "retained-log", CreatedAt: 100, Type: LogTypeConsume}).Error)
	chunks := []LogContentChunk{
		{RequestId: "old-1", Kind: common.RelayContentKindRequest, ChunkIndex: 0, CreatedAt: 100},
		{RequestId: "old-2", Kind: common.RelayContentKindRequest, ChunkIndex: 0, CreatedAt: 200},
		{RequestId: "old-3", Kind: common.RelayContentKindResponse, ChunkIndex: 0, CreatedAt: 300},
		{RequestId: "new-1", Kind: common.RelayContentKindResponse, ChunkIndex: 0, CreatedAt: 500},
	}
	require.NoError(t, db.Create(&chunks).Error)

	deleted, err := DeleteExpiredLogContentChunksBatch(context.Background(), 400, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	remainingExpired, err := CountExpiredLogContentChunks(context.Background(), 400)
	require.NoError(t, err)
	assert.Equal(t, int64(1), remainingExpired)

	deleted, err = DeleteExpiredLogContentChunksBatch(context.Background(), 400, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)

	var remainingChunks int64
	require.NoError(t, db.Model(&LogContentChunk{}).Count(&remainingChunks).Error)
	assert.Equal(t, int64(1), remainingChunks)

	var remainingLogs int64
	require.NoError(t, db.Model(&Log{}).Count(&remainingLogs).Error)
	assert.Equal(t, int64(1), remainingLogs)
}

func TestRelayContentIsPersistedAndReadWithoutTruncation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:log-content-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}, &LogContentChunk{}))

	previousLogDB := LOG_DB
	previousLogDatabaseType := common.LogDatabaseType()
	previousEnabled := common.LogRequestResponseEnabled
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	common.LogRequestResponseEnabled = true
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogDatabaseType)
		common.LogRequestResponseEnabled = previousEnabled
	})

	requestId := "req-complete-content"
	requestBody := strings.Repeat("完整请求", 20_000)
	responseBody := strings.Repeat("complete-response", 10_000)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set(common.RequestIdKey, requestId)

	common.BeginRelayContentCapture(context)
	other := NewLogOther()
	if common.ShouldAttachRelayContentToLog(context) {
		other.SetAdmin("request_response_capture", true)
	}
	require.NoError(t, createLog(&Log{
		RequestId: requestId,
		CreatedAt: common.GetTimestamp(),
		Type:      LogTypeConsume,
		Other:     other.JSONString(),
	}))
	context.Writer.Header().Set("Content-Type", "text/event-stream")
	_, err = context.Writer.WriteString(responseBody)
	require.NoError(t, err)

	PersistRelayContentCapture(context)

	requestPage, err := GetLogContentPage(requestId, common.RelayContentKindRequest, 1, 1)
	require.NoError(t, err)
	assert.Greater(t, requestPage.TotalChunks, int64(1))
	assert.Equal(t, int64(len([]byte(requestBody))), requestPage.TotalSize)
	assert.Equal(t, "application/json", requestPage.ContentType)

	var reconstructed []byte
	for page := 1; int64(page) <= requestPage.TotalChunks; page++ {
		contentPage, pageErr := GetLogContentPage(requestId, common.RelayContentKindRequest, page, 1)
		require.NoError(t, pageErr)
		decoded, decodeErr := base64.StdEncoding.DecodeString(contentPage.Chunks[0])
		require.NoError(t, decodeErr)
		reconstructed = append(reconstructed, decoded...)
	}
	assert.Equal(t, []byte(requestBody), reconstructed)

	responsePage, err := GetLogContentPage(requestId, common.RelayContentKindResponse, 1, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(len(responseBody)), responsePage.TotalSize)
	assert.Equal(t, "text/event-stream", responsePage.ContentType)
	responseBytes := make([]byte, 0, len(responseBody))
	for _, chunk := range responsePage.Chunks {
		decoded, decodeErr := base64.StdEncoding.DecodeString(chunk)
		require.NoError(t, decodeErr)
		responseBytes = append(responseBytes, decoded...)
	}
	assert.Equal(t, responseBody, string(responseBytes))
}

func TestRelayContentNotPersistedWhenCaptureDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:log-content-disabled-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}, &LogContentChunk{}))

	previousLogDB := LOG_DB
	previousLogDatabaseType := common.LogDatabaseType()
	previousEnabled := common.LogRequestResponseEnabled
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	common.LogRequestResponseEnabled = false
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogDatabaseType)
		common.LogRequestResponseEnabled = previousEnabled
	})

	requestId := "req-disabled-content"
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set(common.RequestIdKey, requestId)

	common.BeginRelayContentCapture(context)
	other := NewLogOther()
	if common.ShouldAttachRelayContentToLog(context) {
		other.SetAdmin("request_response_capture", true)
	}
	require.NoError(t, createLog(&Log{
		RequestId: requestId,
		CreatedAt: common.GetTimestamp(),
		Type:      LogTypeConsume,
		Other:     other.JSONString(),
	}))
	_, err = context.Writer.WriteString(`{"id":"chatcmpl-1"}`)
	require.NoError(t, err)

	PersistRelayContentCapture(context)

	var chunkCount int64
	require.NoError(t, db.Model(&LogContentChunk{}).Count(&chunkCount).Error)
	assert.Equal(t, int64(0), chunkCount)

	assert.Nil(t, other.Snapshot()[logOtherAdminInfoKey], "disabled capture must not advertise capture")
}

func TestRelayContentNotPersistedWhenDisabledAfterCaptureStarted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:log-content-disable-midflight-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}, &LogContentChunk{}))

	previousLogDB := LOG_DB
	previousLogDatabaseType := common.LogDatabaseType()
	previousEnabled := common.LogRequestResponseEnabled
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	common.LogRequestResponseEnabled = true
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogDatabaseType)
		common.LogRequestResponseEnabled = previousEnabled
	})

	requestId := "req-disable-midflight"
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set(common.RequestIdKey, requestId)

	common.BeginRelayContentCapture(context)
	require.True(t, common.RelayContentCaptureActive(context))
	_, err = context.Writer.WriteString(`{"ok":true}`)
	require.NoError(t, err)

	require.NoError(t, createLog(&Log{
		RequestId: requestId,
		CreatedAt: common.GetTimestamp(),
		Type:      LogTypeConsume,
	}))

	// Simulate admin turning the setting off before the deferred persist runs.
	common.LogRequestResponseEnabled = false
	PersistRelayContentCapture(context)

	var chunkCount int64
	require.NoError(t, db.Model(&LogContentChunk{}).Count(&chunkCount).Error)
	assert.Equal(t, int64(0), chunkCount)
}

// Log schema from v1.0.0-rc.44, before captured content storage was added.
type releasedCaptureLog struct {
	Id                int    `json:"id" gorm:"index:idx_created_at_id,priority:2;index:idx_user_id_id,priority:2"`
	UserId            int    `json:"user_id" gorm:"index;index:idx_user_id_id,priority:1"`
	CreatedAt         int64  `json:"created_at" gorm:"bigint;index:idx_created_at_id,priority:1;index:idx_created_at_type"`
	Type              int    `json:"type" gorm:"index:idx_created_at_type"`
	Content           string `json:"content"`
	Username          string `json:"username" gorm:"index;index:index_username_model_name,priority:2;default:''"`
	TokenName         string `json:"token_name" gorm:"index;default:''"`
	ModelName         string `json:"model_name" gorm:"index;index:index_username_model_name,priority:1;default:''"`
	Quota             int    `json:"quota" gorm:"default:0"`
	PromptTokens      int    `json:"prompt_tokens" gorm:"default:0"`
	CompletionTokens  int    `json:"completion_tokens" gorm:"default:0"`
	UseTime           int    `json:"use_time" gorm:"default:0"`
	IsStream          bool   `json:"is_stream"`
	ChannelId         int    `json:"channel" gorm:"index"`
	ChannelName       string `json:"channel_name" gorm:"->"`
	TokenId           int    `json:"token_id" gorm:"default:0;index"`
	Group             string `json:"group" gorm:"index"`
	Ip                string `json:"ip" gorm:"index;default:''"`
	RequestId         string `json:"request_id,omitempty" gorm:"type:varchar(64);index:idx_logs_request_id;default:''"`
	UpstreamRequestId string `json:"upstream_request_id,omitempty" gorm:"type:varchar(128);index:idx_logs_upstream_request_id;default:''"`
	Other             string `json:"other"`
}
