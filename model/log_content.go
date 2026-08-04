package model

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	logContentRawChunkSize = 32 * 1024
	logContentInsertBatch  = 100
)

var ErrLogContentNotFound = errors.New("log content not found")

func CountExpiredLogContentChunks(ctx context.Context, targetTimestamp int64) (int64, error) {
	var count int64
	err := LOG_DB.WithContext(ctx).Model(&LogContentChunk{}).
		Where("created_at < ?", targetTimestamp).
		Count(&count).Error
	return count, err
}

// DeleteExpiredLogContentChunksBatch removes a bounded set of expired content
// rows on relational databases. ClickHouse performs one synchronous mutation
// because repeated small mutations rewrite data parts and are substantially
// more expensive than one retention pass.
func DeleteExpiredLogContentChunksBatch(ctx context.Context, targetTimestamp int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		count, err := CountExpiredLogContentChunks(ctx, targetTimestamp)
		if err != nil || count == 0 {
			return count, err
		}
		err = LOG_DB.WithContext(ctx).Exec(
			"ALTER TABLE log_content_chunks DELETE WHERE created_at < ? SETTINGS mutations_sync = 1",
			targetTimestamp,
		).Error
		return count, err
	}

	var ids []int64
	err := LOG_DB.WithContext(ctx).Model(&LogContentChunk{}).
		Where("created_at < ?", targetTimestamp).
		Order("id").
		Limit(limit).
		Pluck("id", &ids).Error
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	result := LOG_DB.WithContext(ctx).Where("id IN ?", ids).Delete(&LogContentChunk{})
	return result.RowsAffected, result.Error
}

// LogContentChunk stores request and response bodies separately from the main
// log row. Content is base64 encoded so arbitrary binary responses remain
// portable across SQLite, MySQL, PostgreSQL, and ClickHouse string columns.
type LogContentChunk struct {
	Id          int64  `json:"-"`
	RequestId   string `json:"request_id" gorm:"type:varchar(64);uniqueIndex:idx_log_content_chunk,priority:1;index"`
	Kind        string `json:"kind" gorm:"type:varchar(16);uniqueIndex:idx_log_content_chunk,priority:2"`
	ChunkIndex  int    `json:"chunk_index" gorm:"uniqueIndex:idx_log_content_chunk,priority:3"`
	Content     string `json:"content" gorm:"type:text"`
	ContentType string `json:"content_type" gorm:"type:varchar(255)"`
	Size        int    `json:"size"`
	TotalSize   int64  `json:"total_size" gorm:"bigint"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;index"`
}

type LogContentPage struct {
	Kind        string   `json:"kind"`
	ContentType string   `json:"content_type"`
	Encoding    string   `json:"encoding"`
	Chunks      []string `json:"chunks"`
	Page        int      `json:"page"`
	PageSize    int      `json:"page_size"`
	TotalChunks int64    `json:"total_chunks"`
	TotalSize   int64    `json:"total_size"`
}

func discardRelayContentCapture(c *gin.Context) {
	_ = common.FinishRelayContentCapture(c, nil)
}

// PersistRelayContentCapture runs after the relay handler has finished writing
// its response, ensuring error JSON and the final streaming frame are included.
// When LogRequestResponseEnabled is off or capture was never started for this
// request, this is a no-op and must not write log_content_chunks rows.
func PersistRelayContentCapture(c *gin.Context) {
	if c == nil {
		return
	}
	// Fast path: skip all DB work when capture is disabled or was never armed.
	// Finish still runs so any in-flight temp files are cleaned up, but with a
	// nil persist callback nothing is written — including after a runtime disable.
	if !common.LogRequestResponseEnabled || !common.RelayContentCaptureActive(c) {
		discardRelayContentCapture(c)
		return
	}

	requestId := c.GetString(common.RequestIdKey)
	if requestId == "" {
		discardRelayContentCapture(c)
		return
	}

	var count int64
	if err := LOG_DB.Model(&Log{}).Where("request_id = ?", requestId).Count(&count).Error; err != nil || count == 0 {
		discardRelayContentCapture(c)
		if err != nil {
			logger.LogError(c, "failed to locate usage log for captured content: "+err.Error())
		}
		return
	}

	persist := func(tx *gorm.DB) error {
		return common.FinishRelayContentCapture(c, func(kind string, contentType string, totalSize int64, reader io.Reader) error {
			// Final guard: the flag may flip off between the outer check and the
			// actual insert (admin disable / option sync on another path).
			if !common.LogRequestResponseEnabled {
				return nil
			}
			buffer := make([]byte, logContentRawChunkSize)
			chunks := make([]LogContentChunk, 0, logContentInsertBatch)
			chunkIndex := 0
			for {
				read, readErr := io.ReadFull(reader, buffer)
				if read > 0 {
					chunks = append(chunks, LogContentChunk{
						RequestId:   requestId,
						Kind:        kind,
						ChunkIndex:  chunkIndex,
						Content:     base64.StdEncoding.EncodeToString(buffer[:read]),
						ContentType: contentType,
						Size:        read,
						TotalSize:   totalSize,
						CreatedAt:   common.GetTimestamp(),
					})
					chunkIndex++
				}
				if len(chunks) == logContentInsertBatch || (readErr != nil && len(chunks) > 0) {
					if err := tx.CreateInBatches(&chunks, logContentInsertBatch).Error; err != nil {
						return fmt.Errorf("failed to persist %s log content chunks: %w", kind, err)
					}
					chunks = chunks[:0]
				}
				if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
					break
				}
				if readErr != nil {
					return fmt.Errorf("failed to read complete %s log content: %w", kind, readErr)
				}
			}

			if chunkIndex == 0 {
				emptyChunk := LogContentChunk{
					RequestId:   requestId,
					Kind:        kind,
					ChunkIndex:  0,
					ContentType: contentType,
					TotalSize:   totalSize,
					CreatedAt:   common.GetTimestamp(),
				}
				if err := tx.Create(&emptyChunk).Error; err != nil {
					return fmt.Errorf("failed to persist empty %s log content: %w", kind, err)
				}
			}
			return nil
		})
	}

	var err error
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		err = persist(LOG_DB)
	} else {
		err = LOG_DB.Transaction(persist)
	}
	if err != nil {
		logger.LogError(c, "failed to persist complete relay content: "+err.Error())
	}
}

func GetLogContentPage(requestId string, kind string, page int, pageSize int) (*LogContentPage, error) {
	if kind != common.RelayContentKindRequest && kind != common.RelayContentKindResponse {
		return nil, ErrLogContentNotFound
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 8
	}
	if pageSize > 100 {
		pageSize = 100
	}

	var logCount int64
	if err := LOG_DB.Model(&Log{}).Where("request_id = ?", requestId).Count(&logCount).Error; err != nil {
		return nil, err
	}
	if logCount == 0 {
		return nil, ErrLogContentNotFound
	}

	query := LOG_DB.Model(&LogContentChunk{}).Where("request_id = ? AND kind = ?", requestId, kind)
	var totalChunks int64
	if err := query.Count(&totalChunks).Error; err != nil {
		return nil, err
	}
	if totalChunks == 0 {
		return nil, ErrLogContentNotFound
	}

	var chunks []LogContentChunk
	if err := query.Order("chunk_index asc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&chunks).Error; err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, ErrLogContentNotFound
	}

	contents := make([]string, len(chunks))
	for i := range chunks {
		contents[i] = chunks[i].Content
	}
	return &LogContentPage{
		Kind:        kind,
		ContentType: chunks[0].ContentType,
		Encoding:    "base64",
		Chunks:      contents,
		Page:        page,
		PageSize:    pageSize,
		TotalChunks: totalChunks,
		TotalSize:   chunks[0].TotalSize,
	}, nil
}
