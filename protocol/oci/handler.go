package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
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

	// DefaultTagTTL is the default TTL for tag->digest mappings.
	DefaultTagTTL = 5 * time.Minute
)

// Path patterns for OCI Distribution API — applied to remainder after prefix stripping.
var (
	// Matches {name}/manifests/{reference}
	manifestRemainderRegex = regexp.MustCompile(`^(.+)/manifests/(.+)$`)

	// Matches {name}/blobs/{digest}
	blobRemainderRegex = regexp.MustCompile(`^(.+)/blobs/(sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

// routeResult holds the parsed routing result for an OCI request.
type routeResult struct {
	Registry  *Registry
	Name      string        // upstream image name (without prefix)
	IndexName string        // prefix-scoped name for index operations
	Reference string        // tag or digest (manifest requests)
	Digest    string        // digest string (blob requests)
	TagTTL    time.Duration // effective tag TTL for this request
}

// Handler implements the OCI Distribution v2 protocol as an HTTP handler.
type Handler struct {
	index      *Index
	store      store.Store
	router     *Router
	logger     *slog.Logger
	downloader *download.Downloader

	// Tag TTL - how long before re-validating tag->digest mapping
	tagTTL time.Duration

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

// WithRouter sets the registry router for prefix-based upstream routing.
func WithRouter(router *Router) HandlerOption {
	return func(h *Handler) {
		h.router = router
	}
}

// WithDownloader sets the singleflight downloader for deduplicating concurrent fetches.
func WithDownloader(dl *download.Downloader) HandlerOption {
	return func(h *Handler) {
		h.downloader = dl
	}
}

// WithTagTTL sets the TTL for tag->digest cache entries.
func WithTagTTL(ttl time.Duration) HandlerOption {
	return func(h *Handler) {
		h.tagTTL = ttl
	}
}

// NewHandler creates a new OCI registry handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		index:  index,
		store:  store,
		logger: slog.Default(),
		tagTTL: DefaultTagTTL,
		ctx:    ctx,
		cancel: cancel,
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

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path

	// Handle /v2/ version check
	if path == "/v2/" || path == "/v2" {
		h.handleVersionCheck(w, r)
		return
	}

	// Route to the appropriate registry based on prefix
	if h.router == nil {
		http.Error(w, "no registry configured", http.StatusInternalServerError)
		return
	}

	reg, remainder, err := h.router.Route(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Determine the tag TTL — use registry-specific if set, otherwise handler default
	tagTTL := h.tagTTL
	if reg.TagTTL > 0 {
		tagTTL = reg.TagTTL
	}

	// Handle manifest requests
	if matches := manifestRemainderRegex.FindStringSubmatch(remainder); matches != nil {
		rr := routeResult{
			Registry:  reg,
			Name:      matches[1],
			IndexName: reg.Prefix + "/" + matches[1],
			Reference: matches[2],
			TagTTL:    tagTTL,
		}
		if r.Method == http.MethodHead {
			h.handleHeadManifest(w, r, rr)
		} else {
			h.handleGetManifest(w, r, rr)
		}
		return
	}

	// Handle blob requests
	if matches := blobRemainderRegex.FindStringSubmatch(remainder); matches != nil {
		rr := routeResult{
			Registry:  reg,
			Name:      matches[1],
			IndexName: reg.Prefix + "/" + matches[1],
			Digest:    matches[2],
			TagTTL:    tagTTL,
		}
		if r.Method == http.MethodHead {
			h.handleHeadBlob(w, r, rr)
		} else {
			h.handleGetBlob(w, r, rr)
		}
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// handleVersionCheck handles GET /v2/ requests.
func (h *Handler) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "version")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// handleGetManifest handles GET /v2/{prefix}/{name}/manifests/{reference} requests.
func (h *Handler) handleGetManifest(w http.ResponseWriter, r *http.Request, rr routeResult) {
	telemetry.SetEndpoint(r, "manifest")
	ctx := r.Context()
	logger := h.logger.With("name", rr.IndexName, "reference", rr.Reference, "endpoint", "manifest")

	// Determine if reference is a digest or tag
	isDigest := IsDigestReference(rr.Reference)

	var digest string
	var cachedManifest *CachedManifest

	if isDigest {
		// Direct digest lookup - immutable, no TTL needed
		digest = rr.Reference
		cached, err := h.index.GetManifest(ctx, digest)
		if err == nil {
			cachedManifest = cached
		}
	} else {
		// Tag lookup - check cache with TTL (uses indexName for prefix scoping)
		cachedDigest, refreshedAt, err := h.index.GetTagDigest(ctx, rr.IndexName, rr.Reference)
		if err == nil {
			// Check if tag mapping is still fresh
			if time.Since(refreshedAt) < rr.TagTTL {
				digest = cachedDigest
				cached, err := h.index.GetManifest(ctx, digest)
				if err == nil {
					cachedManifest = cached
					logger.Debug("cache hit", "digest", digest)
				}
			} else {
				// Tag is stale, need to revalidate
				logger.Debug("tag stale, revalidating", "age", time.Since(refreshedAt))
			}
		}
	}

	// If we have a cached manifest, serve it
	if cachedManifest != nil {
		rc, err := h.store.Get(ctx, cachedManifest.ContentHash)
		if err == nil {
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", cachedManifest.MediaType)
			w.Header().Set(DockerContentDigestHeader, cachedManifest.Digest)
			if _, err := io.Copy(w, rc); err != nil {
				logger.Error("failed to stream manifest", "error", err)
			}
			return
		}
		logger.Warn("manifest hash in index but not in store", "hash", cachedManifest.ContentHash.ShortString(), "error", err)
	}

	// Fetch from upstream (using upstream name without prefix)
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("cache miss, fetching from upstream")
	content, mediaType, upstreamDigest, err := rr.Registry.Upstream.FetchManifest(ctx, rr.Name, rr.Reference)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Verify digest if we're fetching by digest
	if isDigest {
		d, err := ParseDigest(rr.Reference)
		if err == nil {
			if err := d.Verify(content); err != nil {
				logger.Error("digest verification failed", "error", err)
				http.Error(w, "digest mismatch", http.StatusBadGateway)
				return
			}
		}
	}

	// Write response
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set(DockerContentDigestHeader, upstreamDigest)
	if _, err := w.Write(content); err != nil {
		logger.Error("failed to write response", "error", err)
		return
	}

	// Cache asynchronously (uses indexName for tag scoping)
	indexName := rr.IndexName
	reference := rr.Reference
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()
		h.cacheManifest(cacheCtx, indexName, reference, upstreamDigest, mediaType, content, logger)
	}()
}

// handleHeadManifest handles HEAD /v2/{prefix}/{name}/manifests/{reference} requests.
func (h *Handler) handleHeadManifest(w http.ResponseWriter, r *http.Request, rr routeResult) {
	telemetry.SetEndpoint(r, "manifest-head")
	ctx := r.Context()
	logger := h.logger.With("name", rr.IndexName, "reference", rr.Reference, "endpoint", "manifest-head")

	isDigest := IsDigestReference(rr.Reference)

	if isDigest {
		// Check cache for digest
		cached, err := h.index.GetManifest(ctx, rr.Reference)
		if err == nil {
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			w.Header().Set("Content-Type", cached.MediaType)
			w.Header().Set(DockerContentDigestHeader, cached.Digest)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", cached.Size))
			w.WriteHeader(http.StatusOK)
			return
		}
	} else {
		// Check tag cache (uses indexName for prefix scoping)
		digest, refreshedAt, err := h.index.GetTagDigest(ctx, rr.IndexName, rr.Reference)
		if err == nil && time.Since(refreshedAt) < rr.TagTTL {
			cached, err := h.index.GetManifest(ctx, digest)
			if err == nil {
				telemetry.SetCacheResult(r, telemetry.CacheHit)
				w.Header().Set("Content-Type", cached.MediaType)
				w.Header().Set(DockerContentDigestHeader, cached.Digest)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", cached.Size))
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}

	// Fetch from upstream (using upstream name without prefix)
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	digest, size, mediaType, err := rr.Registry.Upstream.HeadManifest(ctx, rr.Name, rr.Reference)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream head failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	if mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	w.Header().Set(DockerContentDigestHeader, digest)
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	w.WriteHeader(http.StatusOK)
}

// handleGetBlob handles GET /v2/{prefix}/{name}/blobs/{digest} requests.
func (h *Handler) handleGetBlob(w http.ResponseWriter, r *http.Request, rr routeResult) {
	telemetry.SetEndpoint(r, "blob")
	ctx := r.Context()
	logger := h.logger.With("name", rr.IndexName, "digest", rr.Digest, "prefix", rr.Registry.Prefix, "endpoint", "blob")

	// Check cache first
	cached, err := h.index.GetBlob(ctx, rr.Digest)
	if err == nil {
		rc, err := h.store.Get(ctx, cached.ContentHash)
		if err == nil {
			defer func() { _ = rc.Close() }()
			logger.Debug("cache hit")
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set(DockerContentDigestHeader, rr.Digest)
			if cached.Size > 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", cached.Size))
			}
			if _, err := io.Copy(w, rc); err != nil {
				logger.Error("failed to stream blob", "error", err)
			}
			return
		}
		logger.Warn("blob hash in index but not in store", "hash", cached.ContentHash.ShortString(), "error", err)
	}

	// Fetch from upstream
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("cache miss, fetching from upstream")

	if h.downloader != nil {
		h.handleGetBlobWithDownloader(w, r, rr.Registry.Upstream, rr.Name, rr.Digest, logger)
		return
	}

	h.handleGetBlobDirect(w, r, rr.Registry.Upstream, rr.Name, rr.Digest, logger)
}

// handleGetBlobWithDownloader uses the singleflight downloader to deduplicate concurrent blob fetches.
func (h *Handler) handleGetBlobWithDownloader(w http.ResponseWriter, r *http.Request, upstream *Upstream, name, digestStr string, logger *slog.Logger) {
	key := fmt.Sprintf("oci:blob:%s", digestStr)

	result, _, err := h.downloader.Do(r.Context(), key, func(dlCtx context.Context) (*download.Result, error) {
		return h.fetchAndStoreBlob(dlCtx, upstream, name, digestStr, logger)
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
		Opts: download.ServeOptions{
			ContentType:  "application/octet-stream",
			ExtraHeaders: map[string]string{DockerContentDigestHeader: digestStr},
		},
		Logger: logger,
	})
}

// fetchAndStoreBlob fetches a blob from upstream, verifies its digest, stores in CAFS, and updates the index.
func (h *Handler) fetchAndStoreBlob(ctx context.Context, upstream *Upstream, name, digestStr string, logger *slog.Logger) (*download.Result, error) {
	rc, _, err := upstream.FetchBlob(ctx, name, digestStr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	expectedDigest, err := ParseDigest(digestStr)
	if err != nil {
		return nil, fmt.Errorf("invalid digest: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "oci-blob-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Stream to temp file while computing hashes.
	blake3Reader := contentcache.NewHashingReader(rc)
	digestHasher, err := expectedDigest.NewHasher()
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("unsupported digest algorithm: %w", err)
	}
	teeReader := io.TeeReader(blake3Reader, digestHasher)

	written, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("reading blob: %w", err)
	}
	_ = tmpFile.Close()

	blake3Hash := blake3Reader.Sum()

	// Verify digest.
	computedHex := fmt.Sprintf("%x", digestHasher.Sum(nil))
	if computedHex != expectedDigest.Hex {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("digest verification failed: expected %s, got %s", expectedDigest.Hex, computedHex)
	}

	logger.Debug("digest verified", "digest", digestStr)

	// Delegate storage and index update to cacheBlob, which owns tmpPath cleanup.
	h.cacheBlob(ctx, digestStr, blake3Hash, written, tmpPath, logger)

	return &download.Result{Hash: blake3Hash, Size: written}, nil
}

// handleGetBlobDirect handles blob requests without singleflight deduplication.
// It uses StreamThrough to simultaneously stream the upstream blob to the client
// and a temp file for hashing/verification/caching.
func (h *Handler) handleGetBlobDirect(w http.ResponseWriter, r *http.Request, upstream *Upstream, name, digestStr string, logger *slog.Logger) {
	ctx := r.Context()

	rc, size, err := upstream.FetchBlob(ctx, name, digestStr)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = rc.Close() }()

	expectedDigest, err := ParseDigest(digestStr)
	if err != nil {
		logger.Error("invalid digest", "error", err)
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	digestHasher, err := expectedDigest.NewHasher()
	if err != nil {
		logger.Error("unsupported digest algorithm", "error", err)
		http.Error(w, "unsupported digest", http.StatusBadRequest)
		return
	}

	if err := download.StreamThrough(w, r, rc,
		download.StreamThroughOptions{
			ContentType:   "application/octet-stream",
			ExtraHeaders:  map[string]string{DockerContentDigestHeader: digestStr},
			ContentLength: size,
			ExtraWriters:  []io.Writer{digestHasher},
		},
		func(result *download.StreamThroughResult, tmpPath string) error {
			// Verify OCI digest.
			computedHex := fmt.Sprintf("%x", digestHasher.Sum(nil))
			if computedHex != expectedDigest.Hex {
				logger.Warn("digest verification failed, not caching",
					"expected", expectedDigest.Hex,
					"computed", computedHex,
				)
				return fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest.Hex, computedHex)
			}

			logger.Debug("digest verified", "digest", digestStr)

			// Store in CAFS async — caller owns tmpPath deletion.
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
				defer cancel()
				h.cacheBlob(cacheCtx, digestStr, result.Hash, result.Size, tmpPath, logger)
			}()
			return nil
		},
		logger,
	); err != nil {
		logger.Error("stream-through failed", "error", err)
	}
}

// handleHeadBlob handles HEAD /v2/{prefix}/{name}/blobs/{digest} requests.
func (h *Handler) handleHeadBlob(w http.ResponseWriter, r *http.Request, rr routeResult) {
	telemetry.SetEndpoint(r, "blob-head")
	ctx := r.Context()
	logger := h.logger.With("name", rr.IndexName, "digest", rr.Digest, "prefix", rr.Registry.Prefix, "endpoint", "blob-head")

	// Check cache first
	cached, err := h.index.GetBlob(ctx, rr.Digest)
	if err == nil {
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set(DockerContentDigestHeader, rr.Digest)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", cached.Size))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Check upstream
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	size, err := rr.Registry.Upstream.HeadBlob(ctx, rr.Name, rr.Digest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream head failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(DockerContentDigestHeader, rr.Digest)
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	w.WriteHeader(http.StatusOK)
}

// cacheManifest stores a manifest in the cache.
func (h *Handler) cacheManifest(ctx context.Context, name, reference, digest, mediaType string, content []byte, logger *slog.Logger) {
	// Store content in CAFS
	hash, err := h.store.Put(ctx, strings.NewReader(string(content)))
	if err != nil {
		logger.Error("failed to store manifest", "error", err)
		return
	}

	// Update manifest index
	if err := h.index.PutManifest(ctx, digest, mediaType, hash, int64(len(content))); err != nil {
		logger.Error("failed to update manifest index", "error", err)
		return
	}

	// Update tag mapping if this was a tag reference
	if !IsDigestReference(reference) {
		if err := h.index.SetTagDigest(ctx, name, reference, digest); err != nil {
			logger.Error("failed to update tag index", "error", err)
			return
		}
	}

	logger.Info("cached manifest", "digest", digest, "hash", hash.ShortString(), "size", len(content))
}

// cacheBlob stores a blob in the cache from a temp file.
func (h *Handler) cacheBlob(ctx context.Context, digest string, hash contentcache.Hash, size int64, tmpPath string, logger *slog.Logger) {
	// Clean up temp file when done
	defer func() { _ = os.Remove(tmpPath) }()

	// Open temp file for reading
	tmpFile, err := os.Open(tmpPath)
	if err != nil {
		logger.Error("failed to open temp file for caching", "error", err)
		return
	}
	defer func() { _ = tmpFile.Close() }()

	// Store in CAFS
	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		logger.Error("failed to store blob", "error", err)
		return
	}

	// Verify hash matches what we computed during streaming
	if storedHash != hash {
		logger.Warn("hash mismatch during caching", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	// Update blob index
	if err := h.index.PutBlob(ctx, digest, hash, size); err != nil {
		logger.Error("failed to update blob index", "error", err)
		return
	}

	logger.Info("cached blob", "digest", digest, "hash", hash.ShortString(), "size", size)
}
