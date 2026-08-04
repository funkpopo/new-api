package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogContentCleanupScheduleUsesConfiguredRetentionDays(t *testing.T) {
	previousDays := common.LogRequestResponseRetentionDays
	t.Cleanup(func() { common.LogRequestResponseRetentionDays = previousDays })

	handler := logContentCleanupHandler{}
	common.LogRequestResponseRetentionDays = 0
	assert.False(t, handler.Enabled())

	common.LogRequestResponseRetentionDays = 30
	assert.True(t, handler.Enabled())
	payload, ok := handler.NewPayload().(logContentCleanupPayload)
	require.True(t, ok)
	assert.Equal(t, 30, payload.RetentionDays)
}
