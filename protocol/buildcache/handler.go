package buildcache

import (
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/telemetry"
)

// Handler implements the build cache HTTP API.
//
// GET /buildcache/{actionID}              → streams blob body with X-Output-ID + Content-Length headers
// PUT /buildcache/{actionID}?output_id=  → stores blob, returns 204
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

// NewHandler creates a new build cache handler.
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
	actionID := strings.TrimPrefix(r.URL.Path, "/")
	if actionID == "" {
		http.Error(w, "missing action ID", http.StatusBadRequest)
		return
	}
	if !isValidHex(actionID) {
		http.Error(w, "invalid action ID: must be hex-encoded", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.handleGet(w, r, actionID)
	case http.MethodPut:
		h.handlePut(w, r, actionID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isValidHex reports whether s is a non-empty, even-length hex string.
func isValidHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, actionID string) {
	telemetry.SetEndpoint(r, "get")
	ctx := r.Context()
	logger := h.logger.With("action_id", actionID)

	entry, err := h.index.Get(ctx, actionID)
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
	w.Header().Set("X-Output-ID", entry.OutputID)
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		logger.Error("failed to stream blob", "error", err)
	}
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request, actionID string) {
	telemetry.SetEndpoint(r, "put")
	ctx := r.Context()
	logger := h.logger.With("action_id", actionID)

	outputID := r.URL.Query().Get("output_id")
	if outputID == "" {
		http.Error(w, "missing output_id query parameter", http.StatusBadRequest)
		return
	}

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

	entry := &ActionEntry{
		OutputID: outputID,
		BlobHash: contentcache.NewBlobRef(hash).String(),
		Size:     size,
	}
	if err := h.index.Put(ctx, actionID, entry); err != nil {
		logger.Error("failed to store index entry", "error", err)
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}

	logger.Debug("stored build artifact", "output_id", outputID, "size", size)
	w.WriteHeader(http.StatusNoContent)
}
