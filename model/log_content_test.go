package model

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

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
	other := map[string]interface{}{}
	common.AttachRelayContentToLog(context, other)
	require.NoError(t, createLog(&Log{
		RequestId: requestId,
		CreatedAt: common.GetTimestamp(),
		Type:      LogTypeConsume,
		Other:     common.MapToJsonStr(other),
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
