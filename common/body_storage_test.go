package common

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewReplayableBodyReaderKeepsStorageLifecycleWithCaller(t *testing.T) {
	payload := []byte(`{"model":"test-model","input":"hello"}`)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	defer storage.Close()

	body := NewReplayableBodyReader(storage)
	assert.EqualValues(t, len(payload), body.Size())
	_, exposesCloser := any(body).(io.Closer)
	assert.False(t, exposesCloser, "the request body must not expose the storage closer")

	req, err := http.NewRequest(http.MethodPost, "https://example.com", body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())

	replayBody, err := body.NewReader()
	require.NoError(t, err, "closing the HTTP request body must not close the storage")
	replay, err := io.ReadAll(replayBody)
	require.NoError(t, err)
	require.NoError(t, replayBody.Close())
	assert.Equal(t, payload, replay)

	require.NoError(t, storage.Close())
	_, err = body.NewReader()
	require.ErrorIs(t, err, ErrStorageClosed)
}

func TestReplaceBodyStorageRangesAndLimits(t *testing.T) {
	original := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(original) })
	for _, disk := range []bool{false, true} {
		SetDiskCacheConfig(DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
		for _, tc := range []struct {
			name    string
			changes []BodyReplacement
			limit   int64
			want    string
			tooBig  bool
			invalid bool
		}{
			{name: "unchanged", limit: 6, want: "abcdef"},
			{name: "adjacent insert replace delete", changes: []BodyReplacement{{0, 0, []byte("[")}, {0, 2, []byte("AB")}, {2, 4, nil}, {6, 6, []byte("]")}}, limit: 6, want: "[ABef]"},
			{name: "expansion offset by deletion", changes: []BodyReplacement{{0, 1, []byte("1234")}, {1, 6, nil}}, limit: 6, want: "1234"},
			{name: "empty result", changes: []BodyReplacement{{0, 6, nil}}, limit: 6, want: ""},
			{name: "input limit", limit: 5, tooBig: true},
			{name: "output limit", changes: []BodyReplacement{{6, 6, []byte("!")}}, limit: 6, tooBig: true},
			{name: "overlap", changes: []BodyReplacement{{0, 3, nil}, {2, 4, nil}}, limit: 6, invalid: true},
			{name: "past end", changes: []BodyReplacement{{0, 7, nil}}, limit: 6, invalid: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				storage, err := CreateBodyStorage([]byte("abcdef"))
				require.NoError(t, err)
				defer storage.Close()
				result, err := ReplaceBodyStorage(storage, tc.changes, tc.limit)
				if tc.tooBig || tc.invalid {
					require.Error(t, err)
					assert.Nil(t, result)
					if tc.tooBig {
						assert.ErrorIs(t, err, ErrRequestBodyTooLarge)
					}
					return
				}
				require.NoError(t, err)
				defer result.Close()
				actual, err := result.Bytes()
				require.NoError(t, err)
				assert.Equal(t, tc.want, string(actual))
				reader, err := storage.NewReader()
				require.NoError(t, err)
				defer reader.Close()
				input, err := io.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, "abcdef", string(input), "replacement must not mutate its source")
			})
		}
	}
}

type failingBodyStorageReader struct {
	BodyStorage
	err error
}

func (s failingBodyStorageReader) NewReader() (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(bytes.NewReader([]byte("ab")), bodyReadError{s.err})), nil
}

type bodyReadError struct{ err error }

func (r bodyReadError) Read([]byte) (int, error) { return 0, r.err }

func TestReplaceBodyStorageCleansPartialOutput(t *testing.T) {
	original := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(original) })
	SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
	storage, err := CreateBodyStorage([]byte("abcdef"))
	require.NoError(t, err)
	defer storage.Close()
	before := GetDiskCacheStats()
	readErr := errors.New("body read failed")
	for _, injected := range []error{readErr, io.EOF} {
		result, err := ReplaceBodyStorage(failingBodyStorageReader{storage, injected}, []BodyReplacement{{0, 1, []byte("A")}}, 10)
		require.Error(t, err)
		assert.Nil(t, result)
		if injected == readErr {
			assert.ErrorIs(t, err, readErr)
		} else {
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		}
	}
	after := GetDiskCacheStats()
	assert.Equal(t, before.ActiveDiskFiles, after.ActiveDiskFiles)
	assert.Equal(t, before.CurrentDiskUsageBytes, after.CurrentDiskUsageBytes)
	files, err := filepath.Glob(filepath.Join(GetDiskCachePath(), "new-api-body-cache", "*"))
	require.NoError(t, err)
	assert.Len(t, files, 1, "only the original cache file should remain")

	blockedPath := filepath.Join(t.TempDir(), "file-instead-of-directory")
	require.NoError(t, os.WriteFile(blockedPath, nil, 0600))
	config := GetDiskCacheConfig()
	config.Path = blockedPath
	SetDiskCacheConfig(config)
	result, err := ReplaceBodyStorage(storage, []BodyReplacement{{0, 1, []byte("A")}}, 10)
	require.Error(t, err, "failure to create the output cache must fail closed")
	assert.Nil(t, result)

	config.MaxSizeMB = 0
	SetDiskCacheConfig(config)
	result, err = ReplaceBodyStorage(storage, []BodyReplacement{{0, 1, []byte("A")}}, 10)
	require.NoError(t, err, "unavailable disk capacity keeps the existing memory fallback")
	defer result.Close()
	assert.False(t, result.IsDisk())
	actual, err := result.Bytes()
	require.NoError(t, err)
	assert.Equal(t, "Abcdef", string(actual))
}
