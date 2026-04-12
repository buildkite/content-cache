package goproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/download"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/telemetry"
)

const (
	// cacheTimeout is the maximum time allowed for background caching operations.
	cacheTimeout = 5 * time.Minute
)

// Handler implements the GOPROXY protocol as an HTTP handler.
// It acts as a caching proxy, storing modules in a content-addressable store.
type Handler struct {
	index      *Index
	store      store.Store
	upstream   *Upstream
	logger     *slog.Logger
	downloader *download.Downloader

	// Lifecycle management for background goroutines
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithLogger sets the logger for the handler.
func WithLogger(logger *slog.Logger) HandlerOption {
	return func(h *Handler) {
		h.logger = logger
	}
}

// WithUpstream sets the upstream proxy.
func WithUpstream(upstream *Upstream) HandlerOption {
	return func(h *Handler) {
		h.upstream = upstream
	}
}

// WithDownloader sets the singleflight downloader for deduplicating concurrent fetches.
func WithDownloader(dl *download.Downloader) HandlerOption {
	return func(h *Handler) {
		h.downloader = dl
	}
}

// NewHandler creates a new GOPROXY handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		index:    index,
		store:    store,
		upstream: NewUpstream(),
		logger:   slog.Default(),
		ctx:      ctx,
		cancel:   cancel,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Close shuts down the handler and waits for background operations to complete.
func (h *Handler) Close() {
	h.cancel()
	h.wg.Wait()
}

// startBackgroundCache starts a background caching operation with proper lifecycle management.
func (h *Handler) startBackgroundCache(modulePath, version string, logger *slog.Logger) {
	h.wg.Go(func() {
		// Create a context with timeout, derived from handler's context
		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		h.cacheModuleVersion(ctx, modulePath, version, logger)
	})
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request path
	// Expected formats:
	//   /{module}/@v/list
	//   /{module}/@v/{version}.info
	//   /{module}/@v/{version}.mod
	//   /{module}/@v/{version}.zip
	//   /{module}/@latest

	path := strings.TrimPrefix(r.URL.Path, "/")

	// Handle @latest endpoint
	if strings.HasSuffix(path, "/@latest") {
		modulePath := strings.TrimSuffix(path, "/@latest")
		h.handleLatest(w, r, modulePath)
		return
	}

	// Find @v/ separator
	before, after, ok := strings.Cut(path, "/@v/")
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	encodedModule := before
	remainder := after // Skip "/@v/"

	modulePath, err := decodePath(encodedModule)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid module path: %v", err), http.StatusBadRequest)
		return
	}

	switch {
	case remainder == "list":
		h.handleList(w, r, modulePath)
	case strings.HasSuffix(remainder, ".info"):
		version := strings.TrimSuffix(remainder, ".info")
		h.handleInfo(w, r, modulePath, version)
	case strings.HasSuffix(remainder, ".mod"):
		version := strings.TrimSuffix(remainder, ".mod")
		h.handleMod(w, r, modulePath, version)
	case strings.HasSuffix(remainder, ".zip"):
		version := strings.TrimSuffix(remainder, ".zip")
		h.handleZip(w, r, modulePath, version)
	default:
		http.Error(w, "invalid path", http.StatusBadRequest)
	}
}

// handleList handles /{module}/@v/list requests.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request, modulePath string) {
	telemetry.SetEndpoint(r, "list")
	ctx := r.Context()
	logger := h.logger.With("module", modulePath, "endpoint", "list")

	// Try cache first
	versions, err := h.index.ListVersions(ctx, modulePath)
	if err != nil {
		logger.Error("failed to read cache", "error", err)
	}

	// If we have cached versions, return them
	// In a more sophisticated implementation, we might merge with upstream
	if len(versions) > 0 {
		logger.Debug("cache hit", "versions", len(versions))
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, v := range versions {
			if _, err := fmt.Fprintln(w, v); err != nil {
				logger.Error("failed to write response", "error", err)
				return
			}
		}
		return
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	versions, err = h.upstream.FetchVersionList(ctx, modulePath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, v := range versions {
		if _, err := fmt.Fprintln(w, v); err != nil {
			logger.Error("failed to write response", "error", err)
			return
		}
	}
}

// handleInfo handles /{module}/@v/{version}.info requests.
func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request, modulePath, version string) {
	telemetry.SetEndpoint(r, "info")
	ctx := r.Context()
	logger := h.logger.With("module", modulePath, "version", version, "endpoint", "info")

	// Try cache first
	info, err := h.index.GetVersionInfo(ctx, modulePath, version)
	if err == nil {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeJSON(w, info, logger)
		return
	}
	if !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	info, err = h.upstream.FetchInfo(ctx, modulePath, version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cache the module version (we need all parts, so fetch them)
	h.startBackgroundCache(modulePath, version, logger)

	h.writeJSON(w, info, logger)
}

// handleMod handles /{module}/@v/{version}.mod requests.
func (h *Handler) handleMod(w http.ResponseWriter, r *http.Request, modulePath, version string) {
	telemetry.SetEndpoint(r, "mod")
	ctx := r.Context()
	logger := h.logger.With("module", modulePath, "version", version, "endpoint", "mod")

	// Try cache first
	modFile, err := h.index.GetMod(ctx, modulePath, version)
	if err == nil {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write(modFile); err != nil {
			logger.Error("failed to write response", "error", err)
		}
		return
	}
	if !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	modFile, err = h.upstream.FetchMod(ctx, modulePath, version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cache the module version
	h.startBackgroundCache(modulePath, version, logger)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.Write(modFile); err != nil {
		logger.Error("failed to write response", "error", err)
	}
}

// handleZip handles /{module}/@v/{version}.zip requests.
func (h *Handler) handleZip(w http.ResponseWriter, r *http.Request, modulePath, version string) {
	telemetry.SetEndpoint(r, "zip")
	ctx := r.Context()
	logger := h.logger.With("module", modulePath, "version", version, "endpoint", "zip")

	// Try cache first
	mv, err := h.index.GetModuleVersion(ctx, modulePath, version)
	if err == nil && !mv.ZipHash.IsZero() {
		// Get zip from CAFS
		rc, err := h.store.Get(ctx, mv.ZipHash)
		if err == nil {
			logger.Debug("cache hit")
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", "application/zip")
			if _, err := io.Copy(w, rc); err != nil {
				logger.Error("failed to stream zip", "error", err)
			}
			return
		}
		logger.Warn("zip hash in index but not in store", "hash", mv.ZipHash, "error", err)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	if h.downloader != nil {
		h.handleZipWithDownloader(w, r, modulePath, version, logger)
		return
	}

	h.handleZipDirect(w, r, modulePath, version, logger)
}

// handleZipWithDownloader uses the singleflight downloader to deduplicate concurrent zip fetches.
// Unlike the direct path, this downloads to CAFS first, then serves from the store,
// eliminating the double-fetch that occurs in the legacy path.
func (h *Handler) handleZipWithDownloader(w http.ResponseWriter, r *http.Request, modulePath, version string, logger *slog.Logger) {
	key := fmt.Sprintf("goproxy:zip:%s:%s", modulePath, version)

	result, _, err := h.downloader.Do(r.Context(), key, func(dlCtx context.Context) (*download.Result, error) {
		return h.fetchAndStoreZip(dlCtx, modulePath, version, logger)
	})

	download.HandleResult(download.HandleResultParams{
		Writer:     w,
		Request:    r,
		Downloader: h.downloader,
		Key:        key,
		Result:     result,
		Err:        err,
		Store:      h.store,
		IsNotFound: func(e error) bool { return errors.Is(e, ErrNotFound) },
		Opts:       download.ServeOptions{ContentType: "application/zip"},
		Logger:     logger,
	})
}

// fetchAndStoreZip fetches a module zip, stores it in CAFS, and updates the index.
func (h *Handler) fetchAndStoreZip(ctx context.Context, modulePath, version string, logger *slog.Logger) (*download.Result, error) {
	// Fetch info first (needed for index)
	info, err := h.upstream.FetchInfo(ctx, modulePath, version)
	if err != nil {
		return nil, fmt.Errorf("fetching info: %w", err)
	}

	// Fetch mod file
	modFile, err := h.upstream.FetchMod(ctx, modulePath, version)
	if err != nil {
		return nil, fmt.Errorf("fetching mod: %w", err)
	}

	// Fetch and store zip
	zipReader, err := h.upstream.FetchZip(ctx, modulePath, version)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zipReader.Close() }()

	// Stream through a temp file to track size and compute hash
	tmpFile, err := os.CreateTemp("", "goproxy-zip-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hr := contentcache.NewHashingReader(zipReader)

	size, err := io.Copy(tmpFile, hr)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("reading zip: %w", err)
	}
	zipHash := hr.Sum()

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("storing zip: %w", err)
	}
	_ = tmpFile.Close()

	if storedHash != zipHash {
		logger.Warn("hash mismatch during storage", "expected", zipHash.ShortString(), "got", storedHash.ShortString())
	}

	// Store in index
	mv := &ModuleVersion{
		Info:    *info,
		ZipHash: zipHash,
	}
	if err := h.index.PutModuleVersion(ctx, modulePath, version, mv, modFile); err != nil {
		logger.Error("failed to store module version in index", "error", err)
	} else {
		logger.Info("cached module version", "zip_hash", zipHash.ShortString(), "size", size)
	}

	return &download.Result{Hash: zipHash, Size: size}, nil
}

// handleZipDirect handles zip requests without singleflight deduplication (legacy path).
func (h *Handler) handleZipDirect(w http.ResponseWriter, r *http.Request, modulePath, version string, logger *slog.Logger) {
	ctx := r.Context()

	zipReader, err := h.upstream.FetchZip(ctx, modulePath, version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = zipReader.Close() }()

	// Use a HashingReader to compute hash while streaming
	hr := contentcache.NewHashingReader(zipReader)

	w.Header().Set("Content-Type", "application/zip")

	// Stream to client while computing hash
	if _, err := io.Copy(w, hr); err != nil {
		logger.Error("failed to stream zip", "error", err)
		return
	}

	// Cache the module version asynchronously
	h.startBackgroundCache(modulePath, version, logger)
}

// handleLatest handles /{module}/@latest requests.
func (h *Handler) handleLatest(w http.ResponseWriter, r *http.Request, modulePath string) {
	telemetry.SetEndpoint(r, "latest")
	ctx := r.Context()
	logger := h.logger.With("module", modulePath, "endpoint", "latest")

	// Fetch from upstream (we don't cache @latest as it can change)
	logger.Debug("fetching latest from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)
	info, err := h.upstream.FetchLatest(ctx, modulePath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	h.writeJSON(w, info, logger)
}

// cacheModuleVersion fetches and caches a complete module version.
func (h *Handler) cacheModuleVersion(ctx context.Context, modulePath, version string, logger *slog.Logger) {
	// Check if already cached
	if _, err := h.index.GetModuleVersion(ctx, modulePath, version); err == nil {
		return // Already cached
	}

	logger.Info("caching module version")

	// Fetch info
	info, err := h.upstream.FetchInfo(ctx, modulePath, version)
	if err != nil {
		logger.Error("failed to fetch info for caching", "error", err)
		return
	}

	// Fetch mod
	modFile, err := h.upstream.FetchMod(ctx, modulePath, version)
	if err != nil {
		logger.Error("failed to fetch mod for caching", "error", err)
		return
	}

	// Fetch and store zip
	zipReader, err := h.upstream.FetchZip(ctx, modulePath, version)
	if err != nil {
		logger.Error("failed to fetch zip for caching", "error", err)
		return
	}
	defer func() { _ = zipReader.Close() }()

	zipHash, err := h.store.Put(ctx, zipReader)
	if err != nil {
		logger.Error("failed to store zip", "error", err)
		return
	}

	// Store in index
	mv := &ModuleVersion{
		Info:    *info,
		ZipHash: zipHash,
	}
	if err := h.index.PutModuleVersion(ctx, modulePath, version, mv, modFile); err != nil {
		logger.Error("failed to store module version in index", "error", err)
		return
	}

	logger.Info("successfully cached module version", "zip_hash", zipHash.ShortString())
}

// writeJSON writes a JSON response.
func (h *Handler) writeJSON(w http.ResponseWriter, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("failed to encode JSON response", "error", err)
	}
}
