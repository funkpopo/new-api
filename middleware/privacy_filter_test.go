package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configurePrivacyJSONTest(t testing.TB, disk bool) {
	t.Helper()
	setting := operation_setting.GetPrivacyFilterSetting()
	originalSetting := *setting
	originalCache := common.GetDiskCacheConfig()
	*setting = operation_setting.PrivacyFilterSetting{Enabled: true}
	common.SetDiskCacheConfig(common.DiskCacheConfig{
		Enabled: disk, ThresholdMB: 0, MaxSizeMB: 1024, Path: t.TempDir(),
	})
	t.Cleanup(func() {
		*setting = originalSetting
		common.SetDiskCacheConfig(originalCache)
	})
}

// A disk path must use independent readers, never materialize the whole body.
type privacyStreamingStorage struct{ common.BodyStorage }

func (privacyStreamingStorage) Bytes() ([]byte, error) {
	return nil, errors.New("disk body must not call Bytes")
}

func TestPrivacyJSONPreservesProtocol(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		count int
	}{
		{
			name:  "raw numbers and unknown fields",
			input: ` { "content" : "alice@example.com", "unknown":"bob@example.com", "id":900719925474099312345, "exponent":1e+999, "zero":-0.0, "n":0, "enabled":false, "empty":null } `,
			want:  ` { "content" : "[邮箱]", "unknown":"bob@example.com", "id":900719925474099312345, "exponent":1e+999, "zero":-0.0, "n":0, "enabled":false, "empty":null } `,
			count: 1,
		},
		{
			name:  "nested arrays and object key scope",
			input: `{"INPUT":["alice@example.com",["bob@example.com",0,false],{"other":"keep@example.com","text":"carol@example.com"}],"messages":[{"content":[{"type":"text","text":"dave@example.com"}]}]}`,
			want:  `{"INPUT":["[邮箱]",["[邮箱]",0,false],{"other":"keep@example.com","text":"[邮箱]"}],"messages":[{"content":[{"type":"text","text":"[邮箱]"}]}]}`,
			count: 4,
		},
		{
			name:  "special empty numeric and duplicate keys",
			input: `{"":{"content":"alice@example.com"},"0":{"a.b*?\\#:@|":{"te\u0078t":"bob@example.com"}},"content":"carol@example.com","content":"dave@example.com"}`,
			want:  `{"":{"content":"[邮箱]"},"0":{"a.b*?\\#:@|":{"te\u0078t":"[邮箱]"}},"content":"[邮箱]","content":"[邮箱]"}`,
			count: 4,
		},
		{
			name:  "escaped unicode and string syntax",
			input: `{"text":"\"alice\u0040example.com\"\n\\中文\t😀","raw":"\u4e2d\/文"}`,
			want:  `{"text":"\"[邮箱]\"\n\\中文\t😀","raw":"\u4e2d\/文"}`,
			count: 1,
		},
		{name: "no hits", input: "{\n \"text\" : \"hello\\u0020world\",\"content\":\"\",\"other\":\"alice@example.com\"\n}"},
		{name: "root array", input: `["alice@example.com",{"text":"bob@example.com"}]`, want: `["alice@example.com",{"text":"[邮箱]"}]`, count: 1},
		{name: "root string", input: `"alice@example.com"`},
		{name: "root scalar", input: `false`},
		{name: "empty body", input: ``},
	}
	for _, disk := range []bool{false, true} {
		name := "memory"
		if disk {
			name = "disk"
		}
		t.Run(name, func(t *testing.T) {
			configurePrivacyJSONTest(t, disk)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					want := tc.want
					if tc.count == 0 {
						want = tc.input
					}
					data := []byte(tc.input)
					got, stats, err := service.RedactPrivacyJSON(data)
					require.NoError(t, err)
					assert.Equal(t, want, string(got))
					assert.Equal(t, service.PrivacyFilterStats{Hit: tc.count > 0, Count: tc.count}, stats)
					if tc.count == 0 && len(data) > 0 {
						assert.Same(t, &data[0], &got[0])
					}
					input, err := common.CreateBodyStorage(data)
					require.NoError(t, err)
					defer input.Close()
					require.Equal(t, disk, input.IsDisk())
					if disk {
						input = privacyStreamingStorage{input}
					}
					output, storageStats, err := service.RedactPrivacyJSONStorage(input, 1<<20)
					require.NoError(t, err)
					defer output.Close()
					assert.Equal(t, stats, storageStats)
					if tc.count == 0 {
						assert.Equal(t, input, output)
					}
					reader, err := output.NewReader()
					require.NoError(t, err)
					defer reader.Close()
					actual, err := io.ReadAll(reader)
					require.NoError(t, err)
					assert.Equal(t, want, string(actual))
					assert.EqualValues(t, len(actual), output.Size())
				})
			}
		})
	}
}

func TestPrivacyJSONRejectsInvalidInput(t *testing.T) {
	for _, disk := range []bool{false, true} {
		configurePrivacyJSONTest(t, disk)
		for _, input := range []string{
			` `, `{`, `{"text":"alice@example.com"`, `{"text":"alice@example.com",}`,
			`{"text":"bad\xescape"}`, `{"text":"unterminated}`, `{"x":01}`, `{"x":NaN}`,
			`{"other":"bad\xescape"}`, `{"other":"bad\u00xz"}`, `{"other":"bad\u00"}`,
			`{"x":true,false}`, `{"text":["alice@example.com",]}`, `{"x":[1,2}}`,
			`{"text":"alice@example.com"} false`, `[] garbage`, "{\"text\":\"a\nb\"}",
		} {
			_, _, err := service.RedactPrivacyJSON([]byte(input))
			require.Error(t, err, "input %q", input)
			storage, err := common.CreateBodyStorage([]byte(input))
			require.NoError(t, err)
			output, stats, err := service.RedactPrivacyJSONStorage(storage, 1<<20)
			require.Error(t, err, "disk=%v input=%q", disk, input)
			assert.Nil(t, output)
			assert.Zero(t, stats)
			require.NoError(t, storage.Close())
		}
	}
}

func TestPrivacyJSONChunkBoundaries(t *testing.T) {
	configurePrivacyJSONTest(t, true)
	for _, key := range []string{"text", "other"} {
		prefix := `{"` + key + `":"`
		// Move escapes and their hex digits across the streaming read boundary.
		for split := range 8 {
			payload := []byte(prefix + strings.Repeat(" ", (32<<10)-len(prefix)-split) + `\u0040\"\\\/\b\f\n\r\t alice@example.com"}`)
			want, wantStats, err := service.RedactPrivacyJSON(payload)
			require.NoError(t, err)
			input, err := common.CreateBodyStorage(payload)
			require.NoError(t, err)
			output, stats, err := service.RedactPrivacyJSONStorage(privacyStreamingStorage{input}, 1<<20)
			require.NoError(t, err, "key=%s split=%d", key, split)
			reader, err := output.NewReader()
			require.NoError(t, err)
			actual, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			assert.Equal(t, want, actual)
			assert.Equal(t, wantStats, stats)
			require.NoError(t, output.Close())
			require.NoError(t, input.Close())
		}
	}
}

func TestPrivacyJSONStorageLimitsAndFailures(t *testing.T) {
	for _, disk := range []bool{false, true} {
		configurePrivacyJSONTest(t, disk)
		// The shortest email expands from six UTF-8 bytes to an eight-byte label.
		data := []byte(`{"text":"a@b.co"}`)
		storage, err := common.CreateBodyStorage(data)
		require.NoError(t, err)
		before := common.GetDiskCacheStats()
		for _, limit := range []int64{int64(len(data) - 1), int64(len(data) + 1)} {
			output, stats, err := service.RedactPrivacyJSONStorage(storage, limit)
			require.ErrorIs(t, err, common.ErrRequestBodyTooLarge)
			assert.Nil(t, output)
			assert.Zero(t, stats)
		}
		setting := operation_setting.GetPrivacyFilterSetting()
		setting.GitleaksTOML = filepath.Join(t.TempDir(), "missing.toml")
		output, _, err := service.RedactPrivacyJSONStorage(storage, 1<<20)
		require.Error(t, err)
		assert.Nil(t, output)
		after := common.GetDiskCacheStats()
		assert.Equal(t, before.ActiveDiskFiles, after.ActiveDiskFiles)
		assert.Equal(t, before.CurrentDiskUsageBytes, after.CurrentDiskUsageBytes)
		reader, err := storage.NewReader()
		require.NoError(t, err, "failure must leave the original storage usable")
		actual, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		assert.Equal(t, data, actual)
		setting.Enabled = false
		output, stats, err := service.RedactPrivacyJSONStorage(storage, 1<<20)
		require.NoError(t, err)
		assert.Equal(t, storage, output)
		assert.Zero(t, stats)
		require.NoError(t, storage.Close())
	}
}

func TestPrivacyJSONMiddlewareStorageLifecycle(t *testing.T) {
	configurePrivacyJSONTest(t, true)
	for _, text := range []string{"alice@example.com", "hello"} {
		payload := []byte(`{"text":"` + text + `"}`)
		storage, err := common.CreateBodyStorage(payload)
		require.NoError(t, err)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(common.KeyBodyStorage, privacyStreamingStorage{storage})
		c.Set(service.PrivacyFilterRedactedCountContextKey, 2)
		require.NoError(t, filterJSONBody(c))
		want := `{"text":"hello"}`
		if text != "hello" {
			want = `{"text":"[邮箱]"}`
			_, err = storage.NewReader()
			require.ErrorIs(t, err, common.ErrStorageClosed)
			assert.Equal(t, 3, c.GetInt(service.PrivacyFilterRedactedCountContextKey))
		} else {
			assert.Equal(t, 2, c.GetInt(service.PrivacyFilterRedactedCountContextKey))
		}
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Equal(t, want, string(body))
		assert.EqualValues(t, len(body), c.Request.ContentLength)
		require.NoError(t, c.Request.Body.Close())
		current, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		require.True(t, current.IsDisk())
		replay, err := current.NewReader()
		require.NoError(t, err)
		replayed, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		assert.Equal(t, body, replayed)
		common.CleanupBodyStorage(c)
		_, err = current.NewReader()
		require.ErrorIs(t, err, common.ErrStorageClosed)
	}
}

func TestPrivacyJSONMiddlewareRejectsOversizedBodies(t *testing.T) {
	configurePrivacyJSONTest(t, true)
	originalMax := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = originalMax })
	for _, cached := range []bool{false, true} {
		for _, payload := range []string{
			`{"text":"` + strings.Repeat("a", 1<<20) + `"}`,
			`{"text":"a@b.co","pad":"` + strings.Repeat(" ", (1<<20)-26) + `"}`,
		} {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
			c.Request.Header.Set("Content-Type", "application/json")
			if cached {
				storage, err := common.CreateBodyStorage([]byte(payload))
				require.NoError(t, err)
				c.Set(common.KeyBodyStorage, storage)
			} else {
				c.Request.ContentLength = -1 // Chunked input must obey the same limit.
			}
			PrivacyFilter()(c)
			assert.True(t, c.IsAborted())
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), "privacy_filter_failed")
			assert.Zero(t, c.GetInt(service.PrivacyFilterRedactedCountContextKey))
			common.CleanupBodyStorage(c)
		}
	}
}

// Keep the benchmark's immutable input alive across middleware invocations.
type privacyBenchmarkStorage struct{ common.BodyStorage }

func (privacyBenchmarkStorage) Close() error { return nil }

func BenchmarkPrivacyJSON(b *testing.B) {
	for _, sample := range []struct {
		name    string
		payload string
	}{
		{"many_hits", `{"messages":[` + strings.TrimSuffix(strings.Repeat(`{"content":"contact alice@example.com"},`, 256), ",") + `]}`},
		{"large_many_hits", `{"messages":[` + strings.TrimSuffix(strings.Repeat(`{"content":"contact alice@example.com","padding":"`+strings.Repeat("x", 4096)+`"},`, 256), ",") + `]}`},
		{"no_hits", `{"messages":[` + strings.TrimSuffix(strings.Repeat(`{"content":"ordinary message"},`, 256), ",") + `]}`},
		{"large_document", `{"data":[` + strings.TrimSuffix(strings.Repeat(`{"value":"`+strings.Repeat("x", 4096)+`"},`, 4096), ",") + `],"text":"alice@example.com"}`},
		{"large_attachment", `{"data":"` + strings.Repeat("x", 16<<20) + `","text":"alice@example.com"}`},
	} {
		for _, disk := range []bool{false, true} {
			mode := "memory"
			if disk {
				mode = "disk"
			}
			b.Run(sample.name+"/"+mode, func(b *testing.B) {
				configurePrivacyJSONTest(b, disk)
				payload := []byte(sample.payload)
				storage, err := common.CreateBodyStorage(payload)
				require.NoError(b, err)
				defer storage.Close()
				input := privacyBenchmarkStorage{storage}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
				c.Request.Header.Set("Content-Type", "application/json")
				// Warm the compiled rules before measuring.
				_, _, err = service.RedactPrivacyText("hello")
				require.NoError(b, err)
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				for range b.N {
					c.Set(common.KeyBodyStorage, input)
					if err := filterJSONBody(c); err != nil {
						b.Fatal(err)
					}
					current, err := common.GetBodyStorage(c)
					if err != nil {
						b.Fatal(err)
					}
					if current != input {
						current.Close()
					}
				}
			})
		}
	}
}
