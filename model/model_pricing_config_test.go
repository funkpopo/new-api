package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: legacy databases may hold an empty string for pricing option
// rows (e.g. CompletionRatio = ""). Previously readModelPricingMaps failed
// with "CompletionRatio: unexpected end of JSON input", breaking the model
// pricing settings page. Empty values must fall back to engine defaults.
func TestReadModelPricingMapsWithLegacyEmptyOptionValues(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&Option{}))
	t.Cleanup(func() {
		require.NoError(t, DB.Where("1 = 1").Delete(&Option{}).Error)
	})

	require.NoError(t, DB.Create(&Option{Key: "CompletionRatio", Value: ""}).Error)
	require.NoError(t, DB.Create(&Option{Key: "ModelRatio", Value: "   "}).Error)
	require.NoError(t, DB.Create(&Option{Key: "ModelPrice", Value: `{"gpt-4o":2.5}`}).Error)

	values, err := readModelPricingMaps(DB)
	require.NoError(t, err)

	// Empty/blank values keep engine defaults exactly (no extra entries).
	defaults := defaultPricingMaps()
	assert.Equal(t, defaults["CompletionRatio"], values["CompletionRatio"])
	assert.Equal(t, defaults["ModelRatio"], values["ModelRatio"])
	// Valid values are parsed.
	assert.Equal(t, map[string]any{"gpt-4o": 2.5}, values["ModelPrice"])

	// The pricing snapshot used by the settings page must not fail either.
	snapshot, err := GetModelPricingSnapshot(nil)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
}
