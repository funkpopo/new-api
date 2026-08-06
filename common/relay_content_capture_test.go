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

func TestRelayContentCaptureRecordsResponsesSSEWrittenWithGinRender(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousEnabled := LogRequestResponseEnabled
	LogRequestResponseEnabled = true
	t.Cleanup(func() { LogRequestResponseEnabled = previousEnabled })

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString(`{"model":"codex","stream":true}`))
	BeginRelayContentCapture(context)

	events := []string{
		"event: response.output_text.delta\n",
		`data: {"type":"response.output_text.delta","delta":"完整结果"}`,
		"event: response.completed\n",
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}
	for _, event := range events {
		context.Render(-1, CustomEvent{Data: event})
		context.Writer.Flush()
	}

	var capturedResponse []byte
	err := FinishRelayContentCapture(context, func(kind string, _ string, _ int64, reader io.Reader) error {
		if kind != RelayContentKindResponse {
			return nil
		}
		var readErr error
		capturedResponse, readErr = io.ReadAll(reader)
		return readErr
	})
	require.NoError(t, err)
	assert.NotEmpty(t, capturedResponse)
	assert.Equal(t, recorder.Body.Bytes(), capturedResponse)
	assert.Contains(t, string(capturedResponse), "完整结果")
	assert.Contains(t, string(capturedResponse), "response.completed")
}

func TestRelayContentCaptureDoesNotStartWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousEnabled := LogRequestResponseEnabled
	LogRequestResponseEnabled = false
	t.Cleanup(func() { LogRequestResponseEnabled = previousEnabled })

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))

	BeginRelayContentCapture(context)
	assert.False(t, RelayContentCaptureActive(context))

	other := map[string]interface{}{}
	AttachRelayContentToLog(context, other)
	_, hasAdminInfo := other["admin_info"]
	assert.False(t, hasAdminInfo)

	persistCalled := false
	err := FinishRelayContentCapture(context, func(string, string, int64, io.Reader) error {
		persistCalled = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, persistCalled)
}

func TestRelayContentCaptureDiscardsWhenDisabledAfterBegin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousEnabled := LogRequestResponseEnabled
	LogRequestResponseEnabled = true
	t.Cleanup(func() { LogRequestResponseEnabled = previousEnabled })

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	context.Request.Header.Set("Content-Type", "application/json")

	BeginRelayContentCapture(context)
	require.True(t, RelayContentCaptureActive(context))
	_, err := context.Writer.WriteString(`{"ok":true}`)
	require.NoError(t, err)

	// Admin disables capture before the request finishes.
	LogRequestResponseEnabled = false

	other := map[string]interface{}{}
	AttachRelayContentToLog(context, other)
	_, hasAdminInfo := other["admin_info"]
	assert.False(t, hasAdminInfo, "usage log must not advertise capture after disable")

	persistCalled := false
	err = FinishRelayContentCapture(context, func(string, string, int64, io.Reader) error {
		persistCalled = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, persistCalled, "disabled capture must not persist request/response bodies")
}
