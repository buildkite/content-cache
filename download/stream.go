package download

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	contentcache "github.com/buildkite/content-cache"
)

// StreamThroughOptions configures a stream-through operation.
type StreamThroughOptions struct {
	ContentType   string
	ExtraHeaders  map[string]string
	ContentLength int64       // from upstream; -1 if unknown
	ExtraWriters  []io.Writer // protocol-specific hashers (must not return errors)
}

// StreamThroughResult is returned after streaming completes.
type StreamThroughResult struct {
	Hash contentcache.Hash // BLAKE3 content hash
	Size int64             // total bytes transferred
}

// StreamThrough simultaneously streams upstream content to the HTTP client AND
// to a temp file for hashing/verification/storage. This avoids the two-pass
// pattern (download → temp → serve) by using io.TeeReader to write to both
// destinations in a single pass.
//
// Temp file lifecycle:
//   - StreamThrough creates the temp file.
//   - If onComplete returns an error or is not called (stream failure), StreamThrough deletes it.
//   - If onComplete returns nil, the caller owns deletion (enabling async CAFS storage).
//
// ExtraWriters must not return errors (e.g., hash.Hash implementations).
func StreamThrough(
	w http.ResponseWriter,
	r *http.Request,
	upstream io.Reader,
	opts StreamThroughOptions,
	onComplete func(result *StreamThroughResult, tmpPath string) error,
	logger *slog.Logger,
) error {
	tmpFile, err := os.CreateTemp("", "download-stream-*")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// callerOwnsTmp is set to true only when onComplete returns nil.
	callerOwnsTmp := false
	defer func() {
		_ = tmpFile.Close()
		if !callerOwnsTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	// Build the write pipeline: temp file + any extra writers (protocol hashers).
	writers := make([]io.Writer, 0, 1+len(opts.ExtraWriters))
	writers = append(writers, tmpFile)
	writers = append(writers, opts.ExtraWriters...)
	multiW := io.MultiWriter(writers...)

	// Wrap upstream in a HashingReader for BLAKE3, then tee to the multi-writer.
	blake3Reader := contentcache.NewHashingReader(upstream)
	teeReader := io.TeeReader(blake3Reader, multiW)

	// Write response headers before streaming body.
	w.Header().Set("Content-Type", opts.ContentType)
	if opts.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", opts.ContentLength))
	}
	for k, v := range opts.ExtraHeaders {
		w.Header().Set(k, v)
	}

	// For HEAD requests, respond with headers only — don't download from upstream.
	// The caller already has the metadata needed for HEAD responses.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}

	n, copyErr := io.Copy(w, teeReader)

	if copyErr != nil {
		if n == 0 {
			// No bytes written to the response body yet, so headers haven't been
			// committed. http.Error will call WriteHeader(502), overriding the
			// implicit 200 and the previously set headers (Content-Type, etc.).
			http.Error(w, "upstream error", http.StatusBadGateway)
		} else {
			// Bytes already sent to client — can't change the response, just log.
			logger.Error("stream interrupted after partial write",
				"bytes_written", n,
				"error", copyErr,
			)
		}
		return fmt.Errorf("streaming: %w", copyErr)
	}

	// Content-length mismatch detection.
	if opts.ContentLength > 0 && n != opts.ContentLength {
		logger.Error("content-length mismatch",
			"expected", opts.ContentLength,
			"actual", n,
		)
		// Content already sent to client — can't roll back.
		return fmt.Errorf("content-length mismatch: expected %d, got %d", opts.ContentLength, n)
	}

	result := &StreamThroughResult{
		Hash: blake3Reader.Sum(),
		Size: n,
	}

	if err := onComplete(result, tmpPath); err != nil {
		logger.Warn("onComplete callback failed, not caching",
			"error", err,
		)
		// Content already sent to client successfully — return nil since the
		// client got what it needed, even though caching failed.
		return nil
	}

	callerOwnsTmp = true
	return nil
}
