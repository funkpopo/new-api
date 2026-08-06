package common

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/gin-gonic/gin"
)

const relayContentCaptureContextKey = "relay_content_capture"

const (
	RelayContentKindRequest  = "request"
	RelayContentKindResponse = "response"
)

type relayContentCapture struct {
	mu             sync.Mutex
	requestStorage BodyStorage
	requestType    string
	responseFile   *os.File
	responsePath   string
	responseWriter *relayContentResponseWriter
	persisted      bool
}

type relayContentResponseWriter struct {
	gin.ResponseWriter
	mu       sync.Mutex
	file     *os.File
	size     int64
	writeErr error
}

func (w *relayContentResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written, err := w.ResponseWriter.Write(data)
	if written > 0 && w.writeErr == nil {
		captured, captureErr := w.file.Write(data[:written])
		w.size += int64(captured)
		if captureErr != nil {
			w.writeErr = captureErr
		} else if captured != written {
			w.writeErr = io.ErrShortWrite
		}
	}
	return written, err
}

func (w *relayContentResponseWriter) WriteString(data string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written, err := w.ResponseWriter.WriteString(data)
	if written > 0 && w.writeErr == nil {
		captured, captureErr := io.WriteString(w.file, data[:written])
		w.size += int64(captured)
		if captureErr != nil {
			w.writeErr = captureErr
		} else if captured != written {
			w.writeErr = io.ErrShortWrite
		}
	}
	return written, err
}

// RelayContentCaptureActive reports whether this request currently has an
// in-progress request/response capture attached to the gin context.
func RelayContentCaptureActive(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, exists := c.Get(relayContentCaptureContextKey)
	if !exists {
		return false
	}
	capture, ok := value.(*relayContentCapture)
	return ok && capture != nil
}

// BeginRelayContentCapture starts lossless request/response capture for a relay.
// The response is spooled to a private temporary file so large and streaming
// responses do not have to remain in memory.
func BeginRelayContentCapture(c *gin.Context) {
	if !LogRequestResponseEnabled || c == nil || c.Request == nil {
		return
	}
	if RelayContentCaptureActive(c) {
		return
	}

	requestStorage, err := GetBodyStorage(c)
	if err != nil {
		SysError(fmt.Sprintf("failed to initialize relay request capture: %v", err))
		return
	}
	responseFile, err := os.CreateTemp("", "relay-content-*.tmp")
	if err != nil {
		SysError(fmt.Sprintf("failed to initialize relay response capture: %v", err))
		return
	}
	responsePath := responseFile.Name()

	writer := &relayContentResponseWriter{
		ResponseWriter: c.Writer,
		file:           responseFile,
	}
	capture := &relayContentCapture{
		requestStorage: requestStorage,
		requestType:    c.GetHeader("Content-Type"),
		responseFile:   responseFile,
		responsePath:   responsePath,
		responseWriter: writer,
	}
	c.Writer = writer
	c.Set(relayContentCaptureContextKey, capture)
}

// AttachRelayContentToLog marks the admin-only portion of a usage log so the
// UI knows that the request and response are available from the chunk API.
func AttachRelayContentToLog(c *gin.Context, other map[string]interface{}) {
	if c == nil || other == nil {
		return
	}
	// Re-check the runtime flag so a disable that lands after Begin still
	// prevents the usage log from advertising capturable content.
	if !LogRequestResponseEnabled || !RelayContentCaptureActive(c) {
		return
	}

	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok || adminInfo == nil {
		adminInfo = map[string]interface{}{}
		other["admin_info"] = adminInfo
	}
	adminInfo["request_response_capture"] = true
}

// FinishRelayContentCapture exposes both complete streams to persist and then
// removes the temporary response file. The callback is invoked at most once.
// If capture was never started, or LogRequestResponseEnabled was turned off
// before finish, captured data is discarded and persist is not called.
func FinishRelayContentCapture(c *gin.Context, persist func(kind string, contentType string, size int64, reader io.Reader) error) error {
	if c == nil {
		return nil
	}
	value, exists := c.Get(relayContentCaptureContextKey)
	if !exists {
		return nil
	}
	capture, ok := value.(*relayContentCapture)
	if !ok || capture == nil {
		return nil
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.persisted {
		return nil
	}
	capture.persisted = true
	defer func() {
		_ = capture.responseFile.Close()
		_ = os.Remove(capture.responsePath)
	}()

	// Honor a runtime disable even for in-flight requests: once the admin turns
	// capture off, do not write any further request/response bodies to storage.
	if !LogRequestResponseEnabled || persist == nil {
		return nil
	}

	capture.responseWriter.mu.Lock()
	defer capture.responseWriter.mu.Unlock()
	if capture.responseWriter.writeErr != nil {
		return fmt.Errorf("failed to capture complete relay response: %w", capture.responseWriter.writeErr)
	}
	if _, err := capture.requestStorage.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek captured relay request: %w", err)
	}
	if err := persist(RelayContentKindRequest, capture.requestType, capture.requestStorage.Size(), capture.requestStorage); err != nil {
		return err
	}
	_, _ = capture.requestStorage.Seek(0, io.SeekStart)

	if _, err := capture.responseFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek captured relay response: %w", err)
	}
	responseType := capture.responseWriter.Header().Get("Content-Type")
	return persist(RelayContentKindResponse, responseType, capture.responseWriter.size, capture.responseFile)
}
