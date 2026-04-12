package npm

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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
)

// tarballPathRegex matches tarball URLs like:
// /{package}/-/{package}-{version}.tgz
// /@{scope}/{package}/-/{package}-{version}.tgz
var tarballPathRegex = regexp.MustCompile(`^/(.+?)/-/(.+)\.tgz$`)

// Handler implements the NPM registry protocol as an HTTP handler.
type Handler struct {
	index      *Index
	store      store.Store
	router     *Router
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

// WithUpstream sets a single upstream registry (for backward compatibility).
// This creates a router with the given upstream as the fallback.
func WithUpstream(upstream *Upstream) HandlerOption {
	return func(h *Handler) {
		r, _ := NewRouter(nil, WithFallback(upstream))
		h.router = r
	}
}

// WithRouter sets the NPM router for scope-based upstream selection.
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

// NewHandler creates a new NPM registry handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	defaultRouter, _ := NewRouter(nil, WithFallback(NewUpstream()))
	h := &Handler{
		index:  index,
		store:  store,
		router: defaultRouter,
		logger: slog.Default(),
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

	// Check if this is a tarball request
	if matches := tarballPathRegex.FindStringSubmatch(path); matches != nil {
		packageName, err := decodePackageName(matches[1])
		if err != nil {
			http.Error(w, "invalid package name", http.StatusBadRequest)
			return
		}
		tarballName := matches[2]
		h.handleTarball(w, r, packageName, tarballName)
		return
	}

	// Otherwise, it's a metadata request
	// Path is /{package} or /@{scope}/{package}
	packageName, err := decodePackageName(strings.TrimPrefix(path, "/"))
	if err != nil {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	h.handleMetadata(w, r, packageName)
}

// handleMetadata handles package metadata requests.
func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	logger := h.logger.With("package", name, "endpoint", "metadata")

	// Tag request for logging
	telemetry.SetEndpoint(r, "metadata")

	// Check Accept header for abbreviated metadata
	acceptHeader := r.Header.Get("Accept")
	abbreviated := strings.Contains(acceptHeader, "application/vnd.npm.install-v1+json")

	// Try cache first
	cachedMeta, err := h.index.GetPackageMetadata(ctx, name)
	if err == nil {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeMetadataResponse(w, r, name, cachedMeta, abbreviated, logger)
		return
	}
	if !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream: decode directly without buffering raw bytes
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	upstream := h.router.Match(name)
	meta, err := upstream.FetchPackageMetadataDecoded(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Rewrite tarball URLs, then encode the full metadata for caching.
	h.rewriteTarballURLs(r, meta)

	encoded, err := json.Marshal(meta)
	if err != nil {
		logger.Error("failed to encode metadata", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Cache the full URL-rewritten bytes asynchronously. We always cache the
	// full form so that abbreviated and full requests share one cache entry.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()
		if err := h.index.PutPackageMetadata(cacheCtx, name, encoded); err != nil {
			logger.Error("failed to cache metadata", "error", err)
		} else {
			logger.Debug("cached metadata")
		}
	}()

	// Abbreviate only for the response, never for the cached copy.
	response := encoded
	if abbreviated {
		abbrevMeta := h.abbreviateMetadata(meta)
		response, err = json.Marshal(abbrevMeta)
		if err != nil {
			logger.Error("failed to encode abbreviated metadata", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		logger.Error("failed to write response", "error", err)
	}
}

// writeMetadataResponse writes the metadata response, optionally rewriting tarball URLs.
func (h *Handler) writeMetadataResponse(w http.ResponseWriter, r *http.Request, name string, metadata []byte, abbreviated bool, logger *slog.Logger) {
	// Parse metadata to rewrite tarball URLs
	var meta map[string]any
	if err := json.Unmarshal(metadata, &meta); err != nil {
		// If we can't parse, just return as-is
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(metadata); err != nil {
			logger.Error("failed to write response", "error", err)
		}
		return
	}

	// Rewrite tarball URLs to point to our proxy
	h.rewriteTarballURLs(r, meta)

	// If abbreviated response requested, slim down the metadata
	if abbreviated {
		meta = h.abbreviateMetadata(meta)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(meta); err != nil {
		logger.Error("failed to encode JSON response", "error", err)
	}
}

// rewriteTarballURLs rewrites tarball URLs in metadata to point to our proxy.
func (h *Handler) rewriteTarballURLs(r *http.Request, meta map[string]any) {
	versions, ok := meta["versions"].(map[string]any)
	if !ok {
		return
	}

	// Determine our base URL
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	for _, v := range versions {
		version, ok := v.(map[string]any)
		if !ok {
			continue
		}

		dist, ok := version["dist"].(map[string]any)
		if !ok {
			continue
		}

		tarball, ok := dist["tarball"].(string)
		if !ok {
			continue
		}

		// Extract the tarball path from the original URL
		// e.g., https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz
		// becomes http://localhost:8080/npm/lodash/-/lodash-4.17.21.tgz
		if idx := strings.Index(tarball, "/-/"); idx > 0 {
			tarballPath := tarball[idx:] // /-/lodash-4.17.21.tgz
			name := meta["name"].(string)
			dist["tarball"] = fmt.Sprintf("%s/npm/%s%s", baseURL, encodePackageName(name), tarballPath)
		}
	}
}

// abbreviateMetadata creates an abbreviated version of package metadata.
func (h *Handler) abbreviateMetadata(meta map[string]any) map[string]any {
	abbreviated := map[string]any{
		"name": meta["name"],
	}

	if distTags, ok := meta["dist-tags"]; ok {
		abbreviated["dist-tags"] = distTags
	}

	if time, ok := meta["time"]; ok {
		if timeMap, ok := time.(map[string]any); ok {
			if modified, ok := timeMap["modified"]; ok {
				abbreviated["modified"] = modified
			}
		}
	}

	// Slim down versions
	if versions, ok := meta["versions"].(map[string]any); ok {
		abbrevVersions := make(map[string]any)
		for ver, v := range versions {
			version, ok := v.(map[string]any)
			if !ok {
				continue
			}

			abbrevVer := map[string]any{
				"name":    version["name"],
				"version": version["version"],
			}

			// Include only essential fields
			for _, field := range []string{"bin", "dependencies", "devDependencies", "peerDependencies", "optionalDependencies", "engines", "dist", "_hasShrinkwrap"} {
				if val, ok := version[field]; ok {
					abbrevVer[field] = val
				}
			}

			abbrevVersions[ver] = abbrevVer
		}
		abbreviated["versions"] = abbrevVersions
	}

	return abbreviated
}

// handleTarball handles tarball download requests.
func (h *Handler) handleTarball(w http.ResponseWriter, r *http.Request, packageName, tarballName string) {
	ctx := r.Context()
	logger := h.logger.With("package", packageName, "tarball", tarballName, "endpoint", "tarball")

	// Tag request for logging
	telemetry.SetEndpoint(r, "tarball")

	// Extract version from tarball name
	// e.g., "lodash-4.17.21" from "lodash-4.17.21.tgz"
	version := extractVersionFromTarball(packageName, tarballName)

	// Try cache first
	if version != "" {
		hash, err := h.index.GetVersionTarballHash(ctx, packageName, version)
		if err == nil && !hash.IsZero() {
			rc, err := h.store.Get(ctx, hash)
			if err == nil {
				logger.Debug("cache hit", "version", version)
				telemetry.SetCacheResult(r, telemetry.CacheHit)
				defer func() { _ = rc.Close() }()
				w.Header().Set("Content-Type", "application/octet-stream")
				if _, err := io.Copy(w, rc); err != nil {
					logger.Error("failed to stream tarball", "error", err)
				}
				return
			}
			logger.Warn("tarball hash in index but not in store", "hash", hash.ShortString(), "error", err)
		}
	}

	// Fetch from upstream (with singleflight deduplication if downloader is configured)
	logger.Debug("cache miss, fetching from upstream", "version", version)
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	upstream := h.router.Match(packageName)

	if h.downloader != nil {
		h.handleTarballWithDownloader(w, r, packageName, version, upstream, logger)
		return
	}

	h.handleTarballDirect(w, r, packageName, version, upstream, logger)
}

// handleTarballWithDownloader uses the singleflight downloader to deduplicate concurrent fetches.
func (h *Handler) handleTarballWithDownloader(w http.ResponseWriter, r *http.Request, packageName, version string, upstream *Upstream, logger *slog.Logger) {
	key := fmt.Sprintf("npm:tarball:%s:%s", packageName, version)

	result, _, err := h.downloader.Do(r.Context(), key, func(dlCtx context.Context) (*download.Result, error) {
		return h.fetchAndStoreTarball(dlCtx, packageName, version, upstream, logger)
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
		Opts:       download.ServeOptions{ContentType: "application/octet-stream"},
		Logger:     logger,
	})
}

// fetchAndStoreTarball fetches a tarball from upstream, verifies integrity, stores in CAFS, and updates the index.
func (h *Handler) fetchAndStoreTarball(ctx context.Context, packageName, version string, upstream *Upstream, logger *slog.Logger) (*download.Result, error) {
	// Fetch package metadata to get expected shasum for integrity verification
	var expectedShasum string
	var expectedIntegrity string
	if version != "" {
		meta, err := upstream.FetchPackageMetadata(ctx, packageName)
		if err == nil {
			if versionMeta, ok := meta.Versions[version]; ok && versionMeta.Dist != nil {
				expectedShasum = versionMeta.Dist.Shasum
				expectedIntegrity = versionMeta.Dist.Integrity
			}
		} else {
			logger.Warn("failed to fetch metadata for integrity check", "error", err)
		}
	}

	tarballURL := upstream.TarballURL(packageName, version)

	rc, err := upstream.FetchTarball(ctx, tarballURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	// Create temp file
	tmpFile, err := os.CreateTemp("", "npm-tarball-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	// Stream to temp file while computing hashes
	hr := contentcache.NewHashingReader(rc)
	sha1Hash := sha1.New()
	teeReader := io.TeeReader(hr, sha1Hash)

	size, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("reading tarball: %w", err)
	}
	hash := hr.Sum()
	computedShasum := hex.EncodeToString(sha1Hash.Sum(nil))

	// Verify integrity
	if expectedShasum != "" && computedShasum != expectedShasum {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("integrity check failed: expected %s, got %s", expectedShasum, computedShasum)
	}

	if expectedShasum != "" {
		logger.Debug("integrity check passed", "shasum", computedShasum)
	}

	// Seek to beginning for storing in CAFS
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	// Store in CAFS
	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("storing tarball: %w", err)
	}
	_ = tmpFile.Close()

	if storedHash != hash {
		logger.Warn("hash mismatch during storage", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	// Update index
	if err := h.index.SetVersionTarballHash(ctx, packageName, version, hash, size, computedShasum, expectedIntegrity); err != nil {
		logger.Error("failed to update index", "error", err)
	} else {
		logger.Info("cached tarball", "version", version, "hash", hash.ShortString(), "size", size, "shasum", computedShasum)
	}

	return &download.Result{Hash: hash, Size: size}, nil
}

// handleTarballDirect fetches from upstream without singleflight deduplication (legacy path).
func (h *Handler) handleTarballDirect(w http.ResponseWriter, r *http.Request, packageName, version string, upstream *Upstream, logger *slog.Logger) {
	ctx := r.Context()

	// Fetch package metadata to get expected shasum for integrity verification
	var expectedShasum string
	var expectedIntegrity string
	if version != "" {
		meta, err := upstream.FetchPackageMetadata(ctx, packageName)
		if err == nil {
			if versionMeta, ok := meta.Versions[version]; ok && versionMeta.Dist != nil {
				expectedShasum = versionMeta.Dist.Shasum
				expectedIntegrity = versionMeta.Dist.Integrity
			}
		} else {
			logger.Warn("failed to fetch metadata for integrity check", "error", err)
		}
	}

	tarballURL := upstream.TarballURL(packageName, version)

	rc, err := upstream.FetchTarball(ctx, tarballURL)
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

	// Create temp file to avoid memory exhaustion for large tarballs
	tmpFile, err := os.CreateTemp("", "npm-tarball-*")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	// Stream to temp file while computing both content hash and SHA-1 for verification
	hr := contentcache.NewHashingReader(rc)
	sha1Hash := sha1.New()
	teeReader := io.TeeReader(hr, sha1Hash)

	size, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		_ = tmpFile.Close()
		logger.Error("failed to read tarball", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	hash := hr.Sum()
	computedShasum := hex.EncodeToString(sha1Hash.Sum(nil))

	// Verify integrity before serving to client
	if expectedShasum != "" && computedShasum != expectedShasum {
		_ = tmpFile.Close()
		logger.Error("integrity check failed",
			"expected_shasum", expectedShasum,
			"computed_shasum", computedShasum,
		)
		http.Error(w, "integrity check failed", http.StatusBadGateway)
		return
	}

	if expectedShasum != "" {
		logger.Debug("integrity check passed", "shasum", computedShasum)
	}

	// Seek to beginning for sending to client
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		logger.Error("failed to seek temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Write to client
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, tmpFile); err != nil {
		logger.Error("failed to write response", "error", err)
	}
	_ = tmpFile.Close()

	// Cache asynchronously using the computed hash
	h.startBackgroundCacheWithIntegrity(packageName, version, hash, size, tmpPath, computedShasum, expectedIntegrity, logger)
}

// startBackgroundCacheWithIntegrity starts a background caching operation with integrity info.
func (h *Handler) startBackgroundCacheWithIntegrity(packageName, version string, hash contentcache.Hash, size int64, tmpPath, shasum, integrity string, logger *slog.Logger) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		h.cacheTarballWithIntegrity(ctx, packageName, version, hash, size, tmpPath, shasum, integrity, logger)
	}()
}

// cacheTarballWithIntegrity stores a tarball in the cache from a temp file with integrity info.
func (h *Handler) cacheTarballWithIntegrity(ctx context.Context, packageName, version string, hash contentcache.Hash, size int64, tmpPath, shasum, integrity string, logger *slog.Logger) {
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
		logger.Error("failed to store tarball", "error", err)
		return
	}

	// Verify hash matches what we computed during streaming
	if storedHash != hash {
		logger.Warn("hash mismatch during caching", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	// Update index with integrity information
	if err := h.index.SetVersionTarballHash(ctx, packageName, version, hash, size, shasum, integrity); err != nil {
		logger.Error("failed to update index", "error", err)
		return
	}

	logger.Info("cached tarball", "version", version, "hash", hash.ShortString(), "size", size, "shasum", shasum)
}

// extractVersionFromTarball extracts the version from a tarball filename.
// e.g., "lodash-4.17.21" -> "4.17.21"
// e.g., "package-1.0.0-beta.1" -> "1.0.0-beta.1"
func extractVersionFromTarball(packageName, tarballName string) string {
	// For scoped packages, we need the package name without scope
	simpleName := packageName
	if strings.HasPrefix(packageName, "@") {
		parts := strings.SplitN(packageName, "/", 2)
		if len(parts) == 2 {
			simpleName = parts[1]
		}
	}

	// Tarball name format: {name}-{version}
	prefix := simpleName + "-"
	if strings.HasPrefix(tarballName, prefix) {
		return strings.TrimPrefix(tarballName, prefix)
	}

	return ""
}
