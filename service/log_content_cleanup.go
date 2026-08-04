package service

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	logContentCleanupInterval  = 24 * time.Hour
	logContentCleanupBatchSize = 1000
)

type logContentCleanupHandler struct{}

type logContentCleanupPayload struct {
	RetentionDays int `json:"retention_days"`
}

type logContentCleanupResult struct {
	DeletedChunks int64 `json:"deleted_chunks"`
}

func (logContentCleanupHandler) Type() string {
	return model.SystemTaskTypeLogContentCleanup
}

func (logContentCleanupHandler) Enabled() bool {
	return common.LogRequestResponseRetentionDays > 0
}

func (logContentCleanupHandler) Interval() time.Duration {
	return logContentCleanupInterval
}

func (logContentCleanupHandler) NewPayload() any {
	return logContentCleanupPayload{
		RetentionDays: common.LogRequestResponseRetentionDays,
	}
}

func (logContentCleanupHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	days := common.LogRequestResponseRetentionDays
	if days <= 0 {
		finishLogContentCleanupTask(ctx, task, runnerID, 0)
		return
	}
	targetTimestamp := common.GetTimestamp() - int64(days)*24*60*60

	var deleted int64
	for {
		count, err := model.DeleteExpiredLogContentChunksBatch(
			ctx,
			targetTimestamp,
			logContentCleanupBatchSize,
		)
		if err != nil {
			failSystemTask(task, runnerID, err)
			return
		}
		deleted += count
		if count == 0 || count < logContentCleanupBatchSize || common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
			break
		}
	}

	finishLogContentCleanupTask(ctx, task, runnerID, deleted)
}

func finishLogContentCleanupTask(ctx context.Context, task *model.SystemTask, runnerID string, deleted int64) {
	err := model.FinishSystemTask(
		task.TaskID,
		runnerID,
		model.SystemTaskStatusSucceeded,
		logContentCleanupResult{DeletedChunks: deleted},
		"",
	)
	if err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

func init() {
	RegisterSystemTaskHandler(logContentCleanupHandler{})
}
