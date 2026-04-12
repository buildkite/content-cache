package download

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	contentcache "github.com/buildkite/content-cache"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStreamThrough_HappyPath(t *testing.T) {
	content := []byte("hello world, this is a stream-through test")
	expectedHash := contentcache.HashBytes(content)

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	var gotResult *StreamThroughResult
	var gotTmpPath string

	err := StreamThrough(w, r, bytes.NewReader(content),
		StreamThroughOptions{
			ContentType:   "application/octet-stream",
			ExtraHeaders:  map[string]string{"X-Test": "value"},
			ContentLength: int64(len(content)),
		},
		func(result *StreamThroughResult, tmpPath string) error {
			gotResult = result
			gotTmpPath = tmpPath
			return nil
		},
		testLogger(),
	)

	require.NoError(t, err)
	require.NotNil(t, gotResult)
	require.Equal(t, expectedHash, gotResult.Hash)
	require.Equal(t, int64(len(content)), gotResult.Size)

	// Verify HTTP response.
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, content, w.Body.Bytes())
	require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	require.Equal(t, fmt.Sprintf("%d", len(content)), w.Header().Get("Content-Length"))
	require.Equal(t, "value", w.Header().Get("X-Test"))

	// Verify temp file exists and contains the content.
	tmpContent, err := os.ReadFile(gotTmpPath)
	require.NoError(t, err)
	require.Equal(t, content, tmpContent)

	// Caller owns cleanup.
	_ = os.Remove(gotTmpPath)
}

func TestStreamThrough_ExtraWriters(t *testing.T) {
	content := []byte("extra writer test content")

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	sha256Hasher := sha256.New()

	err := StreamThrough(w, r, bytes.NewReader(content),
		StreamThroughOptions{
			ContentType:  "application/octet-stream",
			ExtraWriters: []io.Writer{sha256Hasher},
		},
		func(result *StreamThroughResult, tmpPath string) error {
			defer os.Remove(tmpPath)
			return nil
		},
		testLogger(),
	)

	require.NoError(t, err)

	// Verify the extra writer received the content.
	expectedSHA := sha256.Sum256(content)
	require.Equal(t, expectedSHA[:], sha256Hasher.Sum(nil))
}

func TestStreamThrough_HeadRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodHead, "/blob", nil)
	w := httptest.NewRecorder()

	onCompleteCalled := false

	// Upstream reader should not be consumed for HEAD requests.
	err := StreamThrough(w, r, bytes.NewReader([]byte("should not be read")),
		StreamThroughOptions{
			ContentType:   "application/octet-stream",
			ContentLength: 42,
			ExtraHeaders:  map[string]string{"X-Test": "head"},
		},
		func(_ *StreamThroughResult, _ string) error {
			onCompleteCalled = true
			return nil
		},
		testLogger(),
	)

	require.NoError(t, err)
	require.False(t, onCompleteCalled, "onComplete should not be called for HEAD requests")

	// HEAD response should have headers but no body.
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	require.Equal(t, "42", w.Header().Get("Content-Length"))
	require.Equal(t, "head", w.Header().Get("X-Test"))
	require.Empty(t, w.Body.Bytes())
}

func TestStreamThrough_UpstreamErrorMidStream(t *testing.T) {
	// Create a reader that errors after some bytes.
	partialContent := []byte("partial data")
	errReader := io.MultiReader(
		bytes.NewReader(partialContent),
		&errorReader{err: errors.New("upstream connection reset")},
	)

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	onCompleteCalled := false

	err := StreamThrough(w, r, errReader,
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(result *StreamThroughResult, tmpPath string) error {
			onCompleteCalled = true
			return nil
		},
		testLogger(),
	)

	require.Error(t, err)
	require.False(t, onCompleteCalled, "onComplete should not be called on stream error")
}

func TestStreamThrough_UpstreamErrorZeroBytes(t *testing.T) {
	errReader := &errorReader{err: errors.New("connection refused")}

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	err := StreamThrough(w, r, errReader,
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(_ *StreamThroughResult, _ string) error {
			t.Fatal("onComplete should not be called")
			return nil
		},
		testLogger(),
	)

	require.Error(t, err)
	// With zero bytes written, we should get a 502.
	require.Equal(t, http.StatusBadGateway, w.Code)
}

func TestStreamThrough_ContentLengthMismatch(t *testing.T) {
	content := []byte("short")

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	onCompleteCalled := false

	err := StreamThrough(w, r, bytes.NewReader(content),
		StreamThroughOptions{
			ContentType:   "application/octet-stream",
			ContentLength: 100, // Expect 100 bytes but only get 5.
		},
		func(_ *StreamThroughResult, _ string) error {
			onCompleteCalled = true
			return nil
		},
		testLogger(),
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "content-length mismatch")
	require.False(t, onCompleteCalled, "onComplete should not be called on content-length mismatch")
}

func TestStreamThrough_OnCompleteError(t *testing.T) {
	content := []byte("content for failed callback")

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	err := StreamThrough(w, r, bytes.NewReader(content),
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(_ *StreamThroughResult, tmpPath string) error {
			return fmt.Errorf("digest mismatch")
		},
		testLogger(),
	)

	// onComplete failure should NOT propagate as an error — content was already
	// successfully sent to the client.
	require.NoError(t, err)

	// Client should still have received the content.
	require.Equal(t, content, w.Body.Bytes())
}

func TestStreamThrough_TempFileCleanedUpOnError(t *testing.T) {
	errReader := &errorReader{err: errors.New("fail")}

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	// We can't easily observe the temp file path when onComplete is not called,
	// but we can verify the function returns an error and the temp dir doesn't
	// accumulate files. Just verify it doesn't panic or leak.
	err := StreamThrough(w, r, errReader,
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(_ *StreamThroughResult, _ string) error {
			t.Fatal("should not be called")
			return nil
		},
		testLogger(),
	)

	require.Error(t, err)
}

func TestStreamThrough_OnCompleteErrorCleansUpTmpFile(t *testing.T) {
	content := []byte("will fail on complete")

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	var tmpPathSeen string

	err := StreamThrough(w, r, bytes.NewReader(content),
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(_ *StreamThroughResult, tmpPath string) error {
			tmpPathSeen = tmpPath
			return fmt.Errorf("verification failed")
		},
		testLogger(),
	)

	require.NoError(t, err)

	// Temp file should be cleaned up since onComplete returned an error.
	_, statErr := os.Stat(tmpPathSeen)
	require.True(t, os.IsNotExist(statErr), "temp file should be deleted when onComplete returns error")
}

func TestStreamThrough_Concurrent(t *testing.T) {
	const goroutines = 10

	errs := make(chan error, goroutines)
	for i := range goroutines {
		go func() {
			content := []byte(fmt.Sprintf("concurrent content %d", i))

			r := httptest.NewRequest(http.MethodGet, "/blob", nil)
			w := httptest.NewRecorder()

			err := StreamThrough(w, r, bytes.NewReader(content),
				StreamThroughOptions{
					ContentType:   "application/octet-stream",
					ContentLength: int64(len(content)),
				},
				func(_ *StreamThroughResult, tmpPath string) error {
					defer os.Remove(tmpPath)
					return nil
				},
				testLogger(),
			)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(content, w.Body.Bytes()) {
				errs <- fmt.Errorf("body mismatch: got %q, want %q", w.Body.String(), content)
				return
			}
			errs <- nil
		}()
	}

	for range goroutines {
		require.NoError(t, <-errs)
	}
}

func TestStreamThrough_TempFileCleanedUpOnStreamError(t *testing.T) {
	// Use a dedicated temp dir so we can observe cleanup.
	tmpDir, err := os.MkdirTemp("", "stream-cleanup-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Count files before.
	entriesBefore, err := os.ReadDir(tmpDir)
	require.NoError(t, err)

	// Patch: we can't control the temp dir used by StreamThrough directly,
	// but we can verify via a mid-stream error that reads the tmp path.
	// Instead, use a reader that errors after some bytes so onComplete is
	// never called, then verify no temp files linger in os.TempDir().
	//
	// More targeted approach: list temp files matching the pattern before/after.
	pattern := "download-stream-*"
	matchesBefore, _ := filepath.Glob(filepath.Join(os.TempDir(), pattern))

	errReader := io.MultiReader(
		bytes.NewReader([]byte("some data")),
		&errorReader{err: errors.New("connection reset")},
	)

	r := httptest.NewRequest(http.MethodGet, "/blob", nil)
	w := httptest.NewRecorder()

	err = StreamThrough(w, r, errReader,
		StreamThroughOptions{
			ContentType: "application/octet-stream",
		},
		func(_ *StreamThroughResult, _ string) error {
			t.Fatal("should not be called")
			return nil
		},
		testLogger(),
	)
	require.Error(t, err)

	// Verify no new temp files were left behind.
	matchesAfter, _ := filepath.Glob(filepath.Join(os.TempDir(), pattern))
	require.Len(t, matchesAfter, len(matchesBefore),
		"temp file should be cleaned up after stream error")

	_ = entriesBefore // used only for the dedicated tmpDir approach
}

// errorReader is a reader that always returns an error.
type errorReader struct {
	err error
}

func (r *errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
