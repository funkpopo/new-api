package common

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayContentCapturePreservesCompleteRequestAndResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousEnabled := LogRequestResponseEnabled
	LogRequestResponseEnabled = true
	t.Cleanup(func() { LogRequestResponseEnabled = previousEnabled })

	requestBody := strings.Repeat("请求内容", 30_000)
	responseBody := strings.Repeat("response-content", 20_000)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(requestBody))
	context.Request.Header.Set("Content-Type", "application/json")

	BeginRelayContentCapture(context)
	other := map[string]interface{}{}
	AttachRelayContentToLog(context, other)
	_, err := context.Writer.WriteString(responseBody)
	require.NoError(t, err)

	captured := map[string][]byte{}
	err = FinishRelayContentCapture(context, func(kind string, contentType string, size int64, reader io.Reader) error {
		body, readErr := io.ReadAll(reader)
		if readErr != nil {
			return readErr
		}
		assert.Equal(t, int64(len(body)), size)
		captured[kind] = body
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []byte(requestBody), captured[RelayContentKindRequest])
	assert.Equal(t, []byte(responseBody), captured[RelayContentKindResponse])

	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, adminInfo["request_response_capture"])
}

func TestRelayContentCaptureForwardsExactResponseWhenCaptureIsEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousEnabled := LogRequestResponseEnabled
	LogRequestResponseEnabled = true
	t.Cleanup(func() { LogRequestResponseEnabled = previousEnabled })

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString("{}"))
	BeginRelayContentCapture(context)

	parts := [][]byte{[]byte("data: first\n\n"), []byte("data: second\n\n")}
	for _, part := range parts {
		_, err := context.Writer.Write(part)
		require.NoError(t, err)
	}
	require.Equal(t, "data: first\n\ndata: second\n\n", recorder.Body.String())

	err := FinishRelayContentCapture(context, func(_ string, _ string, _ int64, reader io.Reader) error {
		_, readErr := io.Copy(io.Discard, reader)
		return readErr
	})
	require.NoError(t, err)
}
