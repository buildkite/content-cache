package httpcache

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/telemetry"
)

// Handler implements the HTTP cache protocol used by sccache and Gradle's HTTP Build Cache.
//
// GET      /prefix/{key}  → streams blob body with Content-Length header; 404 on miss
// PUT      /prefix/{key}  → stores blob, returns 204
// MKCOL    /prefix/{key}  → 201 no-op (sccache WebDAV "create directory" before PUT)
// PROPFIND /prefix/       → 207 minimal WebDAV response (sccache connectivity check)
//
// Keys may contain slashes (sccache uses a/b/c/{hash} sharding). Path traversal (..)
// is rejected.
type Handler struct {
	index  *Index
	store  store.Store
	logger *slog.Logger
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithLogger sets the logger for the handler.
func WithLogger(logger *slog.Logger) HandlerOption {
	return func(h *Handler) { h.logger = logger }
}

// NewHandler creates a new HTTP cache handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	h := &Handler{
		index:  index,
		store:  store,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// PROPFIND on the root is a WebDAV connectivity probe from sccache.
	// Respond with a minimal 207 Multi-Status so sccache confirms the server is reachable.
	if r.Method == "PROPFIND" {
		h.handlePropfind(w, r)
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/")
	if !isValidKey(key) {
		http.Error(w, "missing or invalid cache key", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.handleGet(w, r, key)
	case http.MethodPut:
		h.handlePut(w, r, key)
	case "MKCOL":
		// sccache (WebDAV mode) sends MKCOL to create directory prefixes before PUT.
		// Our store is flat, so treat this as a no-op success.
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isValidKey reports whether s is a valid cache key. Keys may contain slashes
// (sccache uses a/b/c/{hash} sharding) but must not be empty or contain ".."
// path traversal segments.
func isValidKey(s string) bool {
	if s == "" {
		return false
	}
	return !slices.Contains(strings.Split(s, "/"), "..")
}

// handlePropfind responds to WebDAV PROPFIND requests with a minimal 207 Multi-Status.
// sccache sends PROPFIND on startup as a connectivity check before issuing GET/PUT/MKCOL.
func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "propfind")
	href := r.URL.Path
	// opendal (used by sccache) requires getlastmodified in PROPFIND responses —
	// it's a non-optional String field and parse_rfc2822("") fails, silently
	// blocking all cache writes.
	lastModified := time.Now().UTC().Format(http.TimeFormat)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>` +
		`<D:multistatus xmlns:D="DAV:">` +
		`<D:response><D:href>` + href + `</D:href>` +
		`<D:propstat><D:prop>` +
		`<D:resourcetype><D:collection/></D:resourcetype>` +
		`<D:getlastmodified>` + lastModified + `</D:getlastmodified>` +
		`</D:prop>` +
		`<D:status>HTTP/1.1 200 OK</D:status></D:propstat>` +
		`</D:response></D:multistatus>`))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	telemetry.SetEndpoint(r, "get")
	ctx := r.Context()
	logger := h.logger.With("key", key)

	entry, err := h.index.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			telemetry.SetCacheResult(r, telemetry.CacheMiss)
			http.NotFound(w, r)
			return
		}
		logger.Error("index get failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ref, err := contentcache.ParseBlobRef(entry.BlobHash)
	if err != nil {
		logger.Error("invalid blob ref in index", "blob_hash", entry.BlobHash, "error", err)
		http.NotFound(w, r)
		return
	}

	rc, err := h.store.Get(ctx, ref.Hash)
	if err != nil {
		logger.Warn("blob missing from store", "blob_hash", entry.BlobHash)
		telemetry.SetCacheResult(r, telemetry.CacheMiss)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = rc.Close() }()

	telemetry.SetCacheResult(r, telemetry.CacheHit)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		logger.Error("failed to stream blob", "error", err)
	}
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	telemetry.SetEndpoint(r, "put")
	ctx := r.Context()
	logger := h.logger.With("key", key)

	hash, err := h.store.Put(ctx, r.Body)
	if err != nil {
		logger.Error("failed to store blob", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	size, err := h.store.Size(ctx, hash)
	if err != nil {
		logger.Error("failed to get blob size after store", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	entry := &CacheEntry{
		BlobHash: contentcache.NewBlobRef(hash).String(),
		Size:     size,
	}
	if err := h.index.Put(ctx, key, entry); err != nil {
		logger.Error("failed to store index entry", "error", err)
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}

	logger.Debug("stored cache entry", "key", key, "size", size)
	w.WriteHeader(http.StatusNoContent)
}
