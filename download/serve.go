package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store"
)

// ServeOptions configures how ServeFromStore writes the HTTP response.
type ServeOptions struct {
	ContentType  string
	ExtraHeaders map[string]string // e.g., Docker-Content-Digest
}

// HandleResultParams bundles the arguments for HandleResult, keeping the
// function signature concise and call sites self-documenting.
type HandleResultParams struct {
	// Writer and Request for the HTTP response.
	Writer  http.ResponseWriter
	Request *http.Request

	// Downloader and Key are used to call Forget on download errors,
	// allowing subsequent requests to retry.
	Downloader *Downloader
	Key        string

	// Result and Err come from Downloader.Do.
	Result *Result
	Err    error

	// Store is the content-addressable storage to read from on success.
	Store store.Store

	// IsNotFound checks whether err is a protocol-specific not-found error.
	// Each protocol provides its own implementation (e.g., errors.Is(err, ErrNotFound)).
	IsNotFound func(error) bool

	// NotFoundHandler is called when IsNotFound returns true. If nil,
	// a default handler writes a standard 404 response.
	NotFoundHandler func()

	// Opts configures Content-Type and any extra response headers.
	Opts ServeOptions

	// Logger for error reporting.
	Logger *slog.Logger
}

// HandleResult processes the result of a Downloader.Do call, handling errors
// and serving content from the store on success. This consolidates the common
// pattern used across all protocol handlers.
func HandleResult(p HandleResultParams) {
	if p.Err != nil {
		if p.IsNotFound(p.Err) {
			p.Downloader.Forget(p.Key)
			if p.NotFoundHandler != nil {
				p.NotFoundHandler()
			} else {
				http.Error(p.Writer, "not found", http.StatusNotFound)
			}
			return
		}
		forgetOnDownloadError(p.Downloader, p.Key, p.Err)
		handleDownloadError(p.Writer, p.Logger, p.Err)
		return
	}

	ServeFromStore(p.Request.Context(), p.Writer, p.Request, p.Store, p.Result, p.Opts, p.Logger)
}

// handleDownloadError writes an appropriate HTTP error response for download
// errors that are not protocol-specific. It handles context cancellation/timeout
// and generic upstream failures.
func handleDownloadError(w http.ResponseWriter, logger *slog.Logger, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, "request timeout", http.StatusGatewayTimeout)
		return
	}
	logger.Error("download failed", "error", err)
	http.Error(w, "upstream error", http.StatusBadGateway)
}

// ServeFromStore retrieves content from the CAFS store and writes it to the
// HTTP response. It sets Content-Type, Content-Length, and any extra headers
// from opts. For HEAD requests, it writes headers but skips the body.
func ServeFromStore(ctx context.Context, w http.ResponseWriter, r *http.Request, s store.Store, result *Result, opts ServeOptions, logger *slog.Logger) {
	rc, err := s.Get(ctx, result.Hash)
	if err != nil {
		logger.Error("failed to read from store after download", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", opts.ContentType)
	if result.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", result.Size))
	}
	for k, v := range opts.ExtraHeaders {
		w.Header().Set(k, v)
	}

	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, rc); err != nil {
			logger.Error("failed to stream response", "error", err)
		}
	}
}

// forgetOnDownloadError calls Forget on the downloader if the error represents
// a real download failure (not a caller context timeout).
func forgetOnDownloadError(d *Downloader, key string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	d.Forget(key)
}

// Hash re-exports contentcache.Hash for convenience in protocol packages.
type Hash = contentcache.Hash
