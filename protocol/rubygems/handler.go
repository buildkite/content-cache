package rubygems

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/download"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/telemetry"
)

const (
	cacheTimeout = 5 * time.Minute

	// maxMetadataSize limits the size of metadata responses (versions, info, specs)
	// to protect against DoS attacks via extremely large responses.
	// 100MB should be sufficient for even the largest metadata files.
	maxMetadataSize = 100 * 1024 * 1024
)

// Handler implements the RubyGems registry API as an HTTP handler.
type Handler struct {
	index       *Index
	store       store.Store
	upstream    *Upstream
	logger      *slog.Logger
	downloader  *download.Downloader
	metadataTTL time.Duration

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

// WithUpstream sets the upstream client.
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

// WithMetadataTTL sets the TTL for cached metadata.
func WithMetadataTTL(ttl time.Duration) HandlerOption {
	return func(h *Handler) {
		h.metadataTTL = ttl
	}
}

// NewHandler creates a new RubyGems handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		index:       index,
		store:       store,
		upstream:    NewUpstream(),
		logger:      slog.Default(),
		metadataTTL: 5 * time.Minute,
		ctx:         ctx,
		cancel:      cancel,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Close shuts down the handler and waits for background operations.
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

	switch {
	// Compact Index API
	case path == "/versions":
		h.handleVersions(w, r)
	case path == "/names":
		h.handleNames(w, r)
	case strings.HasPrefix(path, "/info/"):
		gemName := strings.TrimPrefix(path, "/info/")
		if !IsValidGemName(gemName) {
			http.Error(w, "invalid gem name", http.StatusBadRequest)
			return
		}
		h.handleInfo(w, r, gemName)

	// Legacy Specs API
	case path == "/specs.4.8.gz":
		h.handleSpecs(w, r, "specs")
	case path == "/latest_specs.4.8.gz":
		h.handleSpecs(w, r, "latest_specs")
	case path == "/prerelease_specs.4.8.gz":
		h.handleSpecs(w, r, "prerelease_specs")
	case strings.HasPrefix(path, "/quick/Marshal.4.8/"):
		h.handleGemspec(w, r)

	// Gem downloads
	case strings.HasPrefix(path, "/gems/"):
		h.handleGem(w, r)

	default:
		http.NotFound(w, r)
	}
}

// handleVersions handles GET /versions requests (Compact Index).
func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "versions")
	ctx := r.Context()
	logger := h.logger.With("endpoint", "versions")

	// Try cache first
	cached, content, err := h.index.GetVersions(ctx)
	if err == nil && !h.index.IsExpired(cached.CachedAt, h.metadataTTL) {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeCompactIndex(w, r, content, cached.ETag, cached.ReprDigest, cached.Size)
		return
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Determine if we can do a Range request
	var etag string
	var rangeStart int64
	if cached != nil {
		etag = cached.ETag
		rangeStart = cached.Size
	}

	// Fetch from upstream
	logger.Debug("cache miss or expired, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	resp, err := h.upstream.FetchVersions(ctx, etag, rangeStart)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle different response codes
	switch resp.StatusCode {
	case http.StatusNotModified:
		// Cache is still valid, update timestamp
		if cached != nil {
			cached.UpdatedAt = time.Now()
			h.cacheVersionsAsync(cached, content, logger)
		}
		h.writeCompactIndex(w, r, content, cached.ETag, cached.ReprDigest, cached.Size)
		return

	case http.StatusPartialContent:
		// Append to existing content
		appendData, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
		if err != nil {
			logger.Error("failed to read partial response", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		newContent := make([]byte, len(content)+len(appendData))
		copy(newContent, content)
		copy(newContent[len(content):], appendData)

		// Validate Repr-Digest if available
		if resp.ReprDigest != "" {
			computed := sha256.Sum256(newContent)
			computedB64 := base64.StdEncoding.EncodeToString(computed[:])
			if computedB64 != resp.ReprDigest {
				logger.Error("repr-digest mismatch after append",
					"expected", resp.ReprDigest,
					"computed", computedB64,
				)
				// Refetch full file by returning error
				http.Error(w, "integrity check failed", http.StatusBadGateway)
				return
			}
		}

		newMeta := &CachedVersions{
			ETag:       resp.ETag,
			ReprDigest: resp.ReprDigest,
			Size:       int64(len(newContent)),
			CachedAt:   time.Now(),
			UpdatedAt:  time.Now(),
		}
		h.cacheVersionsAsync(newMeta, newContent, logger)
		h.writeCompactIndex(w, r, newContent, newMeta.ETag, newMeta.ReprDigest, newMeta.Size)
		return

	case http.StatusRequestedRangeNotSatisfiable:
		// Range invalid (monthly rebuild?), pass through to client
		logger.Debug("range not satisfiable, client should retry")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return

	case http.StatusOK:
		// Full response
		newContent, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
		if err != nil {
			logger.Error("failed to read response", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		newMeta := &CachedVersions{
			ETag:       resp.ETag,
			ReprDigest: resp.ReprDigest,
			Size:       int64(len(newContent)),
			CachedAt:   time.Now(),
			UpdatedAt:  time.Now(),
		}
		h.cacheVersionsAsync(newMeta, newContent, logger)
		h.writeCompactIndex(w, r, newContent, newMeta.ETag, newMeta.ReprDigest, newMeta.Size)
		return

	default:
		logger.Error("unexpected upstream status", "status", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

// handleInfo handles GET /info/{gem} requests (Compact Index).
func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request, gem string) {
	telemetry.SetEndpoint(r, "info")
	ctx := r.Context()
	logger := h.logger.With("endpoint", "info", "gem", gem)

	// Try cache first
	cached, content, err := h.index.GetInfo(ctx, gem)
	if err == nil && !h.index.IsExpired(cached.CachedAt, h.metadataTTL) {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeCompactIndex(w, r, content, cached.ETag, cached.ReprDigest, cached.Size)
		return
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	var etag string
	var rangeStart int64
	if cached != nil {
		etag = cached.ETag
		rangeStart = cached.Size
	}

	logger.Debug("cache miss or expired, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	resp, err := h.upstream.FetchInfo(ctx, gem, etag, rangeStart)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if cached != nil {
			cached.UpdatedAt = time.Now()
			h.cacheInfoAsync(gem, cached, content, logger)
		}
		h.writeCompactIndex(w, r, content, cached.ETag, cached.ReprDigest, cached.Size)
		return

	case http.StatusPartialContent:
		appendData, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
		if err != nil {
			logger.Error("failed to read partial response", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		newContent := make([]byte, len(content)+len(appendData))
		copy(newContent, content)
		copy(newContent[len(content):], appendData)

		// Validate Repr-Digest if available
		if resp.ReprDigest != "" {
			computed := sha256.Sum256(newContent)
			computedB64 := base64.StdEncoding.EncodeToString(computed[:])
			if computedB64 != resp.ReprDigest {
				logger.Error("repr-digest mismatch after append",
					"expected", resp.ReprDigest,
					"computed", computedB64,
				)
				http.Error(w, "integrity check failed", http.StatusBadGateway)
				return
			}
		}

		checksums := ParseInfoChecksums(newContent)
		newMeta := &CachedGemInfo{
			Name:       gem,
			ETag:       resp.ETag,
			ReprDigest: resp.ReprDigest,
			Size:       int64(len(newContent)),
			Checksums:  checksums,
			CachedAt:   time.Now(),
			UpdatedAt:  time.Now(),
		}
		h.cacheInfoAsync(gem, newMeta, newContent, logger)
		h.writeCompactIndex(w, r, newContent, newMeta.ETag, newMeta.ReprDigest, newMeta.Size)
		return

	case http.StatusRequestedRangeNotSatisfiable:
		logger.Debug("range not satisfiable")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return

	case http.StatusOK:
		newContent, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
		if err != nil {
			logger.Error("failed to read response", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		checksums := ParseInfoChecksums(newContent)
		newMeta := &CachedGemInfo{
			Name:       gem,
			ETag:       resp.ETag,
			ReprDigest: resp.ReprDigest,
			Size:       int64(len(newContent)),
			Checksums:  checksums,
			CachedAt:   time.Now(),
			UpdatedAt:  time.Now(),
		}
		h.cacheInfoAsync(gem, newMeta, newContent, logger)
		h.writeCompactIndex(w, r, newContent, newMeta.ETag, newMeta.ReprDigest, newMeta.Size)
		return

	default:
		logger.Error("unexpected upstream status", "status", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

// handleNames handles GET /names requests (Compact Index).
func (h *Handler) handleNames(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "names")
	ctx := r.Context()
	logger := h.logger.With("endpoint", "names")

	// Names endpoint is rarely used, just proxy through
	logger.Debug("fetching names from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)

	resp, err := h.upstream.FetchNames(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream error", resp.StatusCode)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Error("failed to stream response", "error", err)
	}
}

// handleSpecs handles specs.4.8.gz requests (Legacy API).
func (h *Handler) handleSpecs(w http.ResponseWriter, r *http.Request, specsType string) {
	telemetry.SetEndpoint(r, specsType)
	ctx := r.Context()
	logger := h.logger.With("endpoint", specsType)

	// Try cache first
	cached, content, err := h.index.GetSpecs(ctx, specsType)
	if err == nil && !h.index.IsExpired(cached.CachedAt, h.metadataTTL) {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeSpecs(w, r, content, cached.ETag)
		return
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	var etag string
	if cached != nil {
		etag = cached.ETag
	}

	logger.Debug("cache miss or expired, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	resp, err := h.upstream.FetchSpecs(ctx, specsType, etag)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if cached != nil {
			cached.UpdatedAt = time.Now()
			h.cacheSpecsAsync(specsType, cached, content, logger)
		}
		h.writeSpecs(w, r, content, cached.ETag)
		return

	case http.StatusOK:
		newContent, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
		if err != nil {
			logger.Error("failed to read response", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		newMeta := &CachedSpecs{
			Type:      specsType,
			ETag:      resp.ETag,
			Size:      int64(len(newContent)),
			CachedAt:  time.Now(),
			UpdatedAt: time.Now(),
		}
		h.cacheSpecsAsync(specsType, newMeta, newContent, logger)
		h.writeSpecs(w, r, newContent, newMeta.ETag)
		return

	default:
		logger.Error("unexpected upstream status", "status", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

// handleGemspec handles quick/Marshal.4.8/*.gemspec.rz requests.
func (h *Handler) handleGemspec(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "gemspec")
	ctx := r.Context()

	// Parse path: /quick/Marshal.4.8/{name}-{version}[-{platform}].gemspec.rz
	path := strings.TrimPrefix(r.URL.Path, "/quick/Marshal.4.8/")
	if !strings.HasSuffix(path, ".gemspec.rz") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	base := strings.TrimSuffix(path, ".gemspec.rz")

	// Parse as if it were a gem filename
	parsed, err := ParseGemFilename(base + ".gem")
	if err != nil {
		h.logger.Warn("failed to parse gemspec filename", "path", path, "error", err)
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	logger := h.logger.With("endpoint", "gemspec", "name", parsed.Name, "version", parsed.Version, "platform", parsed.Platform)

	// Try cache first
	cached, err := h.index.GetGemspec(ctx, parsed.Name, parsed.Version, parsed.Platform)
	if err == nil {
		// Get from CAFS
		rc, err := h.store.Get(ctx, cached.ContentHash)
		if err == nil {
			logger.Debug("cache hit")
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", "application/x-deflate")
			if cached.Size > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(cached.Size, 10))
			}
			if _, err := io.Copy(w, rc); err != nil {
				logger.Error("failed to stream gemspec", "error", err)
			}
			return
		}
		logger.Warn("gemspec in index but not in store", "hash", cached.ContentHash)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	body, size, err := h.upstream.FetchGemspec(ctx, parsed.Name, parsed.Version, parsed.Platform)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = body.Close() }()

	// Stream to temp file for caching
	tmpFile, err := os.CreateTemp("", "gemspec-*.rz")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()

	// cleanupNeeded tracks whether this function should cleanup the temp file.
	// Set to false when responsibility is handed to the async caching goroutine.
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	hr := contentcache.NewHashingReader(body)
	written, err := io.Copy(tmpFile, hr)
	if err != nil {
		logger.Error("failed to read gemspec", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	contentHash := hr.Sum()

	// Seek to beginning for sending to client
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		logger.Error("failed to seek temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Write to client
	w.Header().Set("Content-Type", "application/x-deflate")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, tmpFile); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
	_ = tmpFile.Close()

	// Hand off cleanup responsibility to goroutine
	cleanupNeeded = false
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { _ = os.Remove(tmpPath) }()

		cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		h.cacheGemspec(cacheCtx, parsed, contentHash, written, tmpPath, logger)
	}()
}

// handleGem handles /gems/{filename}.gem requests.
func (h *Handler) handleGem(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "gem")
	ctx := r.Context()

	filename := strings.TrimPrefix(r.URL.Path, "/gems/")
	if !strings.HasSuffix(filename, ".gem") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	logger := h.logger.With("endpoint", "gem", "filename", filename)

	// Try cache first
	cached, err := h.index.GetGem(ctx, filename)
	if err == nil {
		rc, err := h.store.Get(ctx, cached.ContentHash)
		if err == nil {
			logger.Debug("cache hit")
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", "application/octet-stream")
			if cached.Size > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(cached.Size, 10))
			}
			if r.Method != http.MethodHead {
				if _, err := io.Copy(w, rc); err != nil {
					logger.Error("failed to stream gem", "error", err)
				}
			}
			return
		}
		logger.Warn("gem in index but not in store", "hash", cached.ContentHash)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Parse filename for checksum lookup
	parsed, parseErr := ParseGemFilename(filename)
	var expectedSHA256 string
	if parseErr == nil {
		expectedSHA256 = h.lookupGemChecksum(ctx, parsed, logger)
	} else {
		logger.Warn("failed to parse gem filename, skipping integrity check", "error", parseErr)
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	if h.downloader != nil {
		h.handleGemWithDownloader(w, r, filename, parsed, expectedSHA256, logger)
		return
	}

	h.handleGemDirect(w, r, filename, parsed, expectedSHA256, logger)
}

// handleGemWithDownloader uses the singleflight downloader to deduplicate concurrent gem fetches.
func (h *Handler) handleGemWithDownloader(w http.ResponseWriter, r *http.Request, filename string, parsed *ParsedGemFilename, expectedSHA256 string, logger *slog.Logger) {
	key := fmt.Sprintf("rubygems:gem:%s", filename)

	result, _, err := h.downloader.Do(r.Context(), key, func(dlCtx context.Context) (*download.Result, error) {
		return h.fetchAndStoreGem(dlCtx, filename, parsed, expectedSHA256, logger)
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

// fetchAndStoreGem fetches a gem from upstream, verifies integrity, stores in CAFS, and updates the index.
func (h *Handler) fetchAndStoreGem(ctx context.Context, filename string, parsed *ParsedGemFilename, expectedSHA256 string, logger *slog.Logger) (*download.Result, error) {
	body, _, err := h.upstream.FetchGem(ctx, filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	tmpFile, err := os.CreateTemp("", "gem-*.gem")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hr := contentcache.NewHashingReader(body)
	sha256Hash := sha256.New()
	teeReader := io.TeeReader(hr, sha256Hash)

	written, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("reading gem: %w", err)
	}
	contentHash := hr.Sum()
	computedSHA256 := hex.EncodeToString(sha256Hash.Sum(nil))

	if expectedSHA256 != "" && computedSHA256 != expectedSHA256 {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("integrity check failed: expected %s, got %s", expectedSHA256, computedSHA256)
	}

	if expectedSHA256 != "" {
		logger.Debug("integrity check passed", "sha256", computedSHA256)
	}

	// Seek to beginning for storing in CAFS
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("storing gem: %w", err)
	}
	_ = tmpFile.Close()

	if storedHash != contentHash {
		logger.Warn("hash mismatch during storage", "expected", contentHash.ShortString(), "got", storedHash.ShortString())
	}

	gem := &CachedGem{
		Filename:    filename,
		ContentHash: storedHash,
		Size:        written,
		SHA256:      computedSHA256,
		CachedAt:    time.Now(),
	}
	if parsed != nil {
		gem.Name = parsed.Name
		gem.Version = parsed.Version
		gem.Platform = parsed.Platform
	}

	if err := h.index.PutGem(ctx, gem); err != nil {
		logger.Error("failed to store gem metadata", "error", err)
	} else {
		logger.Info("cached gem", "filename", filename, "hash", storedHash.ShortString(), "size", written)
	}

	return &download.Result{Hash: contentHash, Size: written}, nil
}

// handleGemDirect handles gem requests without singleflight deduplication (legacy path).
func (h *Handler) handleGemDirect(w http.ResponseWriter, r *http.Request, filename string, parsed *ParsedGemFilename, expectedSHA256 string, logger *slog.Logger) {
	ctx := r.Context()

	body, _, err := h.upstream.FetchGem(ctx, filename)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = body.Close() }()

	tmpFile, err := os.CreateTemp("", "gem-*.gem")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()

	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	hr := contentcache.NewHashingReader(body)
	sha256Hash := sha256.New()
	teeReader := io.TeeReader(hr, sha256Hash)

	written, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		logger.Error("failed to read gem", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	contentHash := hr.Sum()
	computedSHA256 := hex.EncodeToString(sha256Hash.Sum(nil))

	if expectedSHA256 != "" && computedSHA256 != expectedSHA256 {
		logger.Error("integrity check failed",
			"expected_sha256", expectedSHA256,
			"computed_sha256", computedSHA256,
		)
		http.Error(w, "integrity check failed", http.StatusBadGateway)
		return
	}

	if expectedSHA256 != "" {
		logger.Debug("integrity check passed", "sha256", computedSHA256)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		logger.Error("failed to seek temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(written, 10))
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, tmpFile); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
	_ = tmpFile.Close()

	cleanupNeeded = false
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { _ = os.Remove(tmpPath) }()

		cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		h.cacheGem(cacheCtx, filename, parsed, contentHash, written, computedSHA256, tmpPath, logger)
	}()
}

// lookupGemChecksum looks up the expected SHA256 for a gem from cached /info.
func (h *Handler) lookupGemChecksum(ctx context.Context, parsed *ParsedGemFilename, logger *slog.Logger) string {
	cached, _, err := h.index.GetInfo(ctx, parsed.Name)
	if err != nil {
		logger.Debug("no cached info for gem, cannot verify integrity", "gem", parsed.Name)
		return ""
	}

	key := parsed.VersionPlatformKey()
	if checksum, ok := cached.Checksums[key]; ok {
		return checksum
	}

	logger.Debug("no checksum found for version", "version", key)
	return ""
}

// writeCompactIndex writes a Compact Index response with proper headers.
// CRITICAL: Content is written as opaque bytes, no transformation.
func (h *Handler) writeCompactIndex(w http.ResponseWriter, r *http.Request, content []byte, etag, reprDigest string, size int64) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Accept-Ranges", "bytes")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if reprDigest != "" {
		w.Header().Set("Repr-Digest", "sha-256=:"+reprDigest+":")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(content)), 10))

	if r.Method != http.MethodHead {
		if _, err := w.Write(content); err != nil {
			h.logger.Error("failed to write response", "error", err)
		}
	}
}

// writeSpecs writes a specs.4.8.gz response.
func (h *Handler) writeSpecs(w http.ResponseWriter, r *http.Request, content []byte, etag string) {
	w.Header().Set("Content-Type", "application/gzip")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(content)), 10))

	if r.Method != http.MethodHead {
		if _, err := w.Write(content); err != nil {
			h.logger.Error("failed to write response", "error", err)
		}
	}
}

// Async caching helpers

func (h *Handler) cacheVersionsAsync(meta *CachedVersions, content []byte, logger *slog.Logger) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		if err := h.index.PutVersions(ctx, meta, content); err != nil {
			logger.Error("failed to cache versions", "error", err)
		} else {
			logger.Debug("cached versions", "size", len(content))
		}
	}()
}

func (h *Handler) cacheInfoAsync(gem string, meta *CachedGemInfo, content []byte, logger *slog.Logger) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		if err := h.index.PutInfo(ctx, gem, meta, content); err != nil {
			logger.Error("failed to cache info", "error", err)
		} else {
			logger.Debug("cached info", "gem", gem, "size", len(content))
		}
	}()
}

func (h *Handler) cacheSpecsAsync(specsType string, meta *CachedSpecs, content []byte, logger *slog.Logger) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		if err := h.index.PutSpecs(ctx, specsType, meta, content); err != nil {
			logger.Error("failed to cache specs", "error", err)
		} else {
			logger.Debug("cached specs", "type", specsType, "size", len(content))
		}
	}()
}

func (h *Handler) cacheGemspec(ctx context.Context, parsed *ParsedGemFilename, hash contentcache.Hash, size int64, tmpPath string, logger *slog.Logger) {
	tmpFile, err := os.Open(tmpPath)
	if err != nil {
		logger.Error("failed to open temp file for caching", "error", err)
		return
	}
	defer func() { _ = tmpFile.Close() }()

	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		logger.Error("failed to store gemspec", "error", err)
		return
	}

	if storedHash != hash {
		logger.Warn("hash mismatch during caching", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	spec := &CachedGemspec{
		Name:        parsed.Name,
		Version:     parsed.Version,
		Platform:    parsed.Platform,
		ContentHash: storedHash,
		Size:        size,
		CachedAt:    time.Now(),
	}
	if err := h.index.PutGemspec(ctx, spec); err != nil {
		logger.Error("failed to store gemspec metadata", "error", err)
		return
	}

	logger.Info("cached gemspec", "name", parsed.Name, "version", parsed.Version, "hash", storedHash.ShortString())
}

func (h *Handler) cacheGem(ctx context.Context, filename string, parsed *ParsedGemFilename, hash contentcache.Hash, size int64, sha256sum, tmpPath string, logger *slog.Logger) {
	tmpFile, err := os.Open(tmpPath)
	if err != nil {
		logger.Error("failed to open temp file for caching", "error", err)
		return
	}
	defer func() { _ = tmpFile.Close() }()

	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		logger.Error("failed to store gem", "error", err)
		return
	}

	if storedHash != hash {
		logger.Warn("hash mismatch during caching", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	gem := &CachedGem{
		Filename:    filename,
		ContentHash: storedHash,
		Size:        size,
		SHA256:      sha256sum,
		CachedAt:    time.Now(),
	}
	if parsed != nil {
		gem.Name = parsed.Name
		gem.Version = parsed.Version
		gem.Platform = parsed.Platform
	}

	if err := h.index.PutGem(ctx, gem); err != nil {
		logger.Error("failed to store gem metadata", "error", err)
		return
	}

	logger.Info("cached gem", "filename", filename, "hash", storedHash.ShortString(), "size", size)
}
