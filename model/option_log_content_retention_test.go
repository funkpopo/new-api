package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateLogRequestResponseRetentionDays(t *testing.T) {
	for _, value := range []string{"0", "1", "30", "36500"} {
		require.NoError(t, validateOptionValue("LogRequestResponseRetentionDays", value))
	}

	for _, value := range []string{"-1", "36501", "1.5", "invalid"} {
		assert.Error(t, validateOptionValue("LogRequestResponseRetentionDays", value))
	}
}
