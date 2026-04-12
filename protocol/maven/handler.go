package maven

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 required for Maven protocol compatibility
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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

	// defaultMetadataTTL is the default TTL for maven-metadata.xml caching.
	defaultMetadataTTL = 5 * time.Minute
)

// Regex patterns for parsing Maven repository paths.
var (
	// metadataRegex matches maven-metadata.xml paths:
	// /{groupPath}/{artifactId}/maven-metadata.xml
	metadataRegex = regexp.MustCompile(`^/(.+)/([^/]+)/maven-metadata\.xml$`)

	// metadataChecksumRegex matches maven-metadata.xml checksum paths:
	// /{groupPath}/{artifactId}/maven-metadata.xml.{md5|sha1|sha256|sha512}
	metadataChecksumRegex = regexp.MustCompile(`^/(.+)/([^/]+)/maven-metadata\.xml\.(md5|sha1|sha256|sha512)$`)

	// artifactRegex matches artifact paths:
	// /{groupPath}/{artifactId}/{version}/{artifactId}-{version}[-{classifier}].{extension}
	// Uses non-greedy matching for groupPath to handle nested groups correctly
	artifactRegex = regexp.MustCompile(`^/(.+)/([^/]+)/([^/]+)/([^/]+)$`)
)

// Handler implements the Maven repository protocol as an HTTP handler.
type Handler struct {
	index       *Index
	store       store.Store
	upstream    *Upstream
	logger      *slog.Logger
	downloader  *download.Downloader
	metadataTTL time.Duration

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

// WithUpstream sets the upstream repository.
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

// WithMetadataTTL sets the TTL for maven-metadata.xml caching.
func WithMetadataTTL(ttl time.Duration) HandlerOption {
	return func(h *Handler) {
		h.metadataTTL = ttl
	}
}

// NewHandler creates a new Maven repository handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		index:       index,
		store:       store,
		upstream:    NewUpstream(),
		logger:      slog.Default(),
		metadataTTL: defaultMetadataTTL,
		ctx:         ctx,
		cancel:      cancel,
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

	// Handle root-level files (e.g., /archetype-catalog.xml)
	if path == "/archetype-catalog.xml" {
		h.handleRootFile(w, r, "archetype-catalog.xml")
		return
	}

	// Handle root-level checksum files (e.g., /archetype-catalog.xml.sha1)
	for _, checksumExt := range []string{".md5", ".sha1", ".sha256", ".sha512"} {
		if strings.HasSuffix(path, checksumExt) {
			baseFile := strings.TrimSuffix(path, checksumExt)
			if baseFile == "/archetype-catalog.xml" {
				checksumType := strings.TrimPrefix(checksumExt, ".")
				h.handleRootFileChecksum(w, r, "archetype-catalog.xml", checksumType)
				return
			}
		}
	}

	// Check for metadata checksum request first (more specific pattern)
	if matches := metadataChecksumRegex.FindStringSubmatch(path); matches != nil {
		groupPath := matches[1]
		artifactID := matches[2]
		checksumType := matches[3]
		groupID := pathToGroupID(groupPath)
		h.handleMetadataChecksum(w, r, groupID, artifactID, checksumType)
		return
	}

	// Check for metadata request
	if matches := metadataRegex.FindStringSubmatch(path); matches != nil {
		groupPath := matches[1]
		artifactID := matches[2]
		groupID := pathToGroupID(groupPath)
		h.handleMetadata(w, r, groupID, artifactID)
		return
	}

	// Must be an artifact request
	if matches := artifactRegex.FindStringSubmatch(path); matches != nil {
		groupPath := matches[1]
		artifactID := matches[2]
		version := matches[3]
		filename := matches[4]
		groupID := pathToGroupID(groupPath)

		// Parse the filename to determine if it's a checksum or artifact
		coord, checksumType, err := parseArtifactFilename(artifactID, version, filename)
		if err != nil {
			h.logger.Debug("failed to parse artifact filename", "path", path, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		coord.GroupID = groupID

		if checksumType != "" {
			h.handleArtifactChecksum(w, r, coord, checksumType)
		} else {
			h.handleArtifact(w, r, coord)
		}
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// handleRootFile handles root-level files like archetype-catalog.xml.
func (h *Handler) handleRootFile(w http.ResponseWriter, r *http.Request, filename string) {
	telemetry.SetEndpoint(r, "root-file")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)

	ctx := r.Context()
	logger := h.logger.With("filename", filename, "endpoint", "root-file")

	// Fetch from upstream (no caching for root files as they change frequently)
	logger.Debug("fetching root file from upstream")
	data, err := h.upstream.FetchRootFile(ctx, filename)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	if r.Method != http.MethodHead {
		if _, err := w.Write(data); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// handleRootFileChecksum handles checksums for root-level files.
func (h *Handler) handleRootFileChecksum(w http.ResponseWriter, r *http.Request, filename, checksumType string) {
	telemetry.SetEndpoint(r, "root-file-checksum")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)

	ctx := r.Context()
	logger := h.logger.With("filename", filename, "checksumType", checksumType, "endpoint", "root-file-checksum")

	// Try to fetch checksum file from upstream first
	checksumFile := filename + "." + checksumType
	data, err := h.upstream.FetchRootFile(ctx, checksumFile)
	if err == nil {
		logger.Debug("fetched checksum from upstream")
		w.Header().Set("Content-Type", "text/plain")
		if r.Method != http.MethodHead {
			if _, err := w.Write(data); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		}
		return
	}

	// If checksum file not found, compute from content
	if errors.Is(err, ErrNotFound) {
		logger.Debug("checksum file not found, computing from content")
		content, err := h.upstream.FetchRootFile(ctx, filename)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			logger.Error("upstream fetch failed", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}

		checksum := computeChecksum(content, checksumType)
		w.Header().Set("Content-Type", "text/plain")
		if r.Method != http.MethodHead {
			if _, err := w.Write([]byte(checksum)); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		}
		return
	}

	logger.Error("upstream fetch failed", "error", err)
	http.Error(w, "upstream error", http.StatusBadGateway)
}

// handleMetadata handles maven-metadata.xml requests.
func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request, groupID, artifactID string) {
	telemetry.SetEndpoint(r, "metadata")

	ctx := r.Context()
	logger := h.logger.With("groupId", groupID, "artifactId", artifactID, "endpoint", "metadata")

	// Try cache first
	cached, err := h.index.GetCachedMetadata(ctx, groupID, artifactID)
	if err == nil && !h.index.IsMetadataExpired(cached, h.metadataTTL) {
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		logger.Debug("cache hit")
		w.Header().Set("Content-Type", "application/xml")
		if r.Method != http.MethodHead {
			if _, err = w.Write(cached.Metadata); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		}
		return
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("cache miss, fetching from upstream")
	rawMeta, err := h.upstream.FetchMetadataRaw(ctx, groupID, artifactID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cache metadata asynchronously
	h.wg.Go(func() {
		cacheCtx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()
		meta := &CachedMetadata{
			GroupID:    groupID,
			ArtifactID: artifactID,
			Metadata:   rawMeta,
		}
		if err := h.index.PutCachedMetadata(cacheCtx, meta); err != nil {
			logger.Error("failed to cache metadata", "error", err)
		} else {
			logger.Debug("cached metadata")
		}
	})

	w.Header().Set("Content-Type", "application/xml")
	if r.Method != http.MethodHead {
		if _, err := w.Write(rawMeta); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// handleMetadataChecksum handles maven-metadata.xml checksum requests.
func (h *Handler) handleMetadataChecksum(w http.ResponseWriter, r *http.Request, groupID, artifactID, checksumType string) {
	telemetry.SetEndpoint(r, "metadata-checksum")

	ctx := r.Context()
	logger := h.logger.With("groupId", groupID, "artifactId", artifactID, "checksumType", checksumType, "endpoint", "metadata-checksum")

	// Try to get cached metadata and compute checksum
	cached, err := h.index.GetCachedMetadata(ctx, groupID, artifactID)
	if err == nil && !h.index.IsMetadataExpired(cached, h.metadataTTL) {
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		checksum := computeChecksum(cached.Metadata, checksumType)
		logger.Debug("computed checksum from cache")
		w.Header().Set("Content-Type", "text/plain")
		if r.Method != http.MethodHead {
			if _, err := w.Write([]byte(checksum)); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		}
		return
	}

	// Fetch checksum from upstream - try to get the checksum file directly first
	// If that fails, fall back to computing from metadata
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("fetching metadata for checksum")
	rawMeta, err := h.upstream.FetchMetadataRaw(ctx, groupID, artifactID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	checksum := computeChecksum(rawMeta, checksumType)
	w.Header().Set("Content-Type", "text/plain")
	if r.Method != http.MethodHead {
		if _, err := w.Write([]byte(checksum)); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// handleArtifact handles artifact file requests (JAR, POM, etc.).
func (h *Handler) handleArtifact(w http.ResponseWriter, r *http.Request, coord ArtifactCoordinate) {
	telemetry.SetEndpoint(r, "artifact")

	ctx := r.Context()
	logger := h.logger.With(
		"groupId", coord.GroupID,
		"artifactId", coord.ArtifactID,
		"version", coord.Version,
		"classifier", coord.Classifier,
		"extension", coord.Extension,
		"endpoint", "artifact",
	)

	// Try cache first
	cached, err := h.index.GetCachedArtifact(ctx, coord)
	if err == nil && !cached.Hash.IsZero() {
		rc, err := h.store.Get(ctx, cached.Hash)
		if err == nil {
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			logger.Debug("cache hit")
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", contentTypeForExtension(coord.Extension))
			if cached.Size > 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", cached.Size))
			}
			if r.Method != http.MethodHead {
				if _, err := io.Copy(w, rc); err != nil {
					logger.Error("failed to stream artifact", "error", err)
				}
			}
			return
		}
		logger.Warn("artifact hash in index but not in store", "hash", cached.Hash.ShortString(), "error", err)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}

	// Fetch from upstream
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("cache miss, fetching from upstream")

	if h.downloader != nil {
		h.handleArtifactWithDownloader(w, r, coord, logger)
		return
	}

	h.handleArtifactDirect(w, r, coord, logger)
}

// handleArtifactWithDownloader uses the singleflight downloader to deduplicate concurrent artifact fetches.
func (h *Handler) handleArtifactWithDownloader(w http.ResponseWriter, r *http.Request, coord ArtifactCoordinate, logger *slog.Logger) {
	key := fmt.Sprintf("maven:artifact:%s/%s/%s/%s.%s", coord.GroupID, coord.ArtifactID, coord.Version, coord.Classifier, coord.Extension)

	result, _, err := h.downloader.Do(r.Context(), key, func(dlCtx context.Context) (*download.Result, error) {
		return h.fetchAndStoreArtifact(dlCtx, coord, logger)
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
		Opts:       download.ServeOptions{ContentType: contentTypeForExtension(coord.Extension)},
		Logger:     logger,
	})
}

// fetchAndStoreArtifact fetches an artifact from upstream, verifies integrity, stores in CAFS, and updates the index.
func (h *Handler) fetchAndStoreArtifact(ctx context.Context, coord ArtifactCoordinate, logger *slog.Logger) (*download.Result, error) {
	sha1Checksum, _ := h.upstream.FetchChecksum(ctx, coord, ChecksumSHA1)

	rc, _, err := h.upstream.FetchArtifact(ctx, coord)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	tmpFile, err := os.CreateTemp("", "maven-artifact-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hr := contentcache.NewHashingReader(rc)
	sha1Hash := sha1.New()
	md5Hash := md5.New()
	sha256Hash := sha256.New()
	sha512Hash := sha512.New()
	multiWriter := io.MultiWriter(tmpFile, sha1Hash, md5Hash, sha256Hash, sha512Hash)

	size, err := io.Copy(multiWriter, hr)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("reading artifact: %w", err)
	}
	blake3Hash := hr.Sum()
	computedSHA1 := hex.EncodeToString(sha1Hash.Sum(nil))

	if sha1Checksum != "" && computedSHA1 != sha1Checksum {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("SHA1 integrity check failed: expected %s, got %s", sha1Checksum, computedSHA1)
	}

	if sha1Checksum != "" {
		logger.Debug("integrity check passed", "sha1", computedSHA1)
	}

	// Seek to beginning for storing in CAFS
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("storing artifact: %w", err)
	}
	_ = tmpFile.Close()

	if storedHash != blake3Hash {
		logger.Warn("hash mismatch during storage", "expected", blake3Hash.ShortString(), "got", storedHash.ShortString())
	}

	checksums := Checksums{
		SHA1:   computedSHA1,
		MD5:    hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256: hex.EncodeToString(sha256Hash.Sum(nil)),
		SHA512: hex.EncodeToString(sha512Hash.Sum(nil)),
	}

	artifact := &CachedArtifact{
		GroupID:    coord.GroupID,
		ArtifactID: coord.ArtifactID,
		Version:    coord.Version,
		Classifier: coord.Classifier,
		Extension:  coord.Extension,
		Hash:       blake3Hash,
		Size:       size,
		Checksums:  checksums,
	}
	if err := h.index.PutCachedArtifact(ctx, artifact); err != nil {
		logger.Error("failed to update index", "error", err)
	} else {
		logger.Info("cached artifact", "hash", blake3Hash.ShortString(), "size", size)
	}

	return &download.Result{Hash: blake3Hash, Size: size}, nil
}

// handleArtifactDirect handles artifact requests without singleflight deduplication (legacy path).
func (h *Handler) handleArtifactDirect(w http.ResponseWriter, r *http.Request, coord ArtifactCoordinate, logger *slog.Logger) {
	ctx := r.Context()

	sha1Checksum, _ := h.upstream.FetchChecksum(ctx, coord, ChecksumSHA1)

	rc, _, err := h.upstream.FetchArtifact(ctx, coord)
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

	tmpFile, err := os.CreateTemp("", "maven-artifact-*")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()

	hr := contentcache.NewHashingReader(rc)
	sha1Hash := sha1.New()
	md5Hash := md5.New()
	sha256Hash := sha256.New()
	sha512Hash := sha512.New()
	multiWriter := io.MultiWriter(tmpFile, sha1Hash, md5Hash, sha256Hash, sha512Hash)

	size, err := io.Copy(multiWriter, hr)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		logger.Error("failed to read artifact", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	blake3Hash := hr.Sum()
	computedSHA1 := hex.EncodeToString(sha1Hash.Sum(nil))
	computedMD5 := hex.EncodeToString(md5Hash.Sum(nil))
	computedSHA256 := hex.EncodeToString(sha256Hash.Sum(nil))
	computedSHA512 := hex.EncodeToString(sha512Hash.Sum(nil))

	if sha1Checksum != "" && computedSHA1 != sha1Checksum {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		logger.Error("SHA1 integrity check failed",
			"expected", sha1Checksum,
			"computed", computedSHA1,
		)
		http.Error(w, "integrity check failed", http.StatusBadGateway)
		return
	}

	if sha1Checksum != "" {
		logger.Debug("integrity check passed", "sha1", computedSHA1)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		logger.Error("failed to seek temp file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeForExtension(coord.Extension))
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, tmpFile); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
	_ = tmpFile.Close()

	checksums := Checksums{
		SHA1:   computedSHA1,
		MD5:    computedMD5,
		SHA256: computedSHA256,
		SHA512: computedSHA512,
	}
	h.startBackgroundCache(coord, blake3Hash, size, tmpPath, checksums, logger)
}

// handleArtifactChecksum handles artifact checksum requests.
func (h *Handler) handleArtifactChecksum(w http.ResponseWriter, r *http.Request, coord ArtifactCoordinate, checksumType string) {
	telemetry.SetEndpoint(r, "artifact-checksum")

	ctx := r.Context()
	logger := h.logger.With(
		"groupId", coord.GroupID,
		"artifactId", coord.ArtifactID,
		"version", coord.Version,
		"checksumType", checksumType,
		"endpoint", "artifact-checksum",
	)

	// Try cache first - get cached artifact and return stored checksum
	cached, err := h.index.GetCachedArtifact(ctx, coord)
	if err == nil {
		var checksum string
		switch checksumType {
		case ChecksumMD5:
			checksum = cached.Checksums.MD5
		case ChecksumSHA1:
			checksum = cached.Checksums.SHA1
		case ChecksumSHA256:
			checksum = cached.Checksums.SHA256
		case ChecksumSHA512:
			checksum = cached.Checksums.SHA512
		}
		if checksum != "" {
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			logger.Debug("cache hit for checksum")
			w.Header().Set("Content-Type", "text/plain")
			if r.Method != http.MethodHead {
				if _, err := w.Write([]byte(checksum)); err != nil {
					logger.Error("failed to write response", "error", err)
				}
			}
			return
		}
	}

	// Fetch from upstream
	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	logger.Debug("cache miss, fetching checksum from upstream")
	checksum, err := h.upstream.FetchChecksum(ctx, coord, checksumType)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	if r.Method != http.MethodHead {
		if _, err := w.Write([]byte(checksum)); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// startBackgroundCache starts a background caching operation.
func (h *Handler) startBackgroundCache(coord ArtifactCoordinate, hash contentcache.Hash, size int64, tmpPath string, checksums Checksums, logger *slog.Logger) {
	h.wg.Go(func() {
		ctx, cancel := context.WithTimeout(h.ctx, cacheTimeout)
		defer cancel()

		h.cacheArtifact(ctx, coord, hash, size, tmpPath, checksums, logger)
	})
}

// cacheArtifact stores an artifact in the cache from a temp file.
func (h *Handler) cacheArtifact(ctx context.Context, coord ArtifactCoordinate, hash contentcache.Hash, size int64, tmpPath string, checksums Checksums, logger *slog.Logger) {
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
		logger.Error("failed to store artifact", "error", err)
		return
	}

	// Verify hash matches
	if storedHash != hash {
		logger.Warn("hash mismatch during caching", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	// Update index
	artifact := &CachedArtifact{
		GroupID:    coord.GroupID,
		ArtifactID: coord.ArtifactID,
		Version:    coord.Version,
		Classifier: coord.Classifier,
		Extension:  coord.Extension,
		Hash:       hash,
		Size:       size,
		Checksums:  checksums,
	}
	if err := h.index.PutCachedArtifact(ctx, artifact); err != nil {
		logger.Error("failed to update index", "error", err)
		return
	}

	logger.Info("cached artifact", "hash", hash.ShortString(), "size", size)
}

// parseArtifactFilename parses a Maven artifact filename.
// Returns the artifact coordinate (without groupID) and optionally the checksum type.
// Example filenames:
//   - commons-lang3-3.12.0.jar
//   - commons-lang3-3.12.0-sources.jar
//   - commons-lang3-3.12.0.pom
//   - commons-lang3-3.12.0.jar.sha1
//   - mylib-1.0-20240118.123456-1.jar (snapshot with timestamp)
func parseArtifactFilename(artifactID, version, filename string) (ArtifactCoordinate, string, error) {
	coord := ArtifactCoordinate{
		ArtifactID: artifactID,
		Version:    version,
	}

	// Check for checksum suffix
	checksumType := ""
	for _, ct := range []string{ChecksumMD5, ChecksumSHA1, ChecksumSHA256, ChecksumSHA512} {
		suffix := "." + ct
		if strings.HasSuffix(filename, suffix) {
			checksumType = ct
			filename = strings.TrimSuffix(filename, suffix)
			break
		}
	}

	// Expected base name: {artifactId}-{version}[-{classifier}].{extension}
	// For snapshots, version may be "1.0-SNAPSHOT" but filename uses timestamp like "1.0-20240118.123456-1"
	prefix := artifactID + "-" + version
	remainder := ""

	if strings.HasPrefix(filename, prefix) {
		remainder = strings.TrimPrefix(filename, prefix)
	} else if strings.HasSuffix(version, "-SNAPSHOT") {
		// Try snapshot timestamp format: artifactId-baseVersion-timestamp-buildNumber.ext
		baseVersion := strings.TrimSuffix(version, "-SNAPSHOT")
		snapshotPrefix := artifactID + "-" + baseVersion + "-"
		if strings.HasPrefix(filename, snapshotPrefix) {
			// Extract the rest and find the extension
			rest := strings.TrimPrefix(filename, snapshotPrefix)
			// Format: YYYYMMDD.HHMMSS-buildNum[-classifier].ext
			// Find the extension by looking for last dot after the timestamp pattern
			if idx := findSnapshotExtensionIndex(rest); idx > 0 {
				remainder = rest[idx:]
			}
		}
	}

	if remainder == "" && !strings.HasPrefix(filename, prefix) {
		return coord, "", fmt.Errorf("filename does not match expected pattern: %s", filename)
	}

	if remainder == "" {
		return coord, "", fmt.Errorf("filename missing extension: %s", filename)
	}

	// Check for classifier or extension
	switch {
	case strings.HasPrefix(remainder, "-"):
		// Has classifier: -{classifier}.{extension}
		remainder = strings.TrimPrefix(remainder, "-")
		lastDot := strings.LastIndex(remainder, ".")
		if lastDot <= 0 {
			return coord, "", fmt.Errorf("invalid classifier/extension format: %s", filename)
		}
		coord.Classifier = remainder[:lastDot]
		coord.Extension = remainder[lastDot+1:]
	case strings.HasPrefix(remainder, "."):
		// No classifier: .{extension}
		coord.Extension = strings.TrimPrefix(remainder, ".")
	default:
		return coord, "", fmt.Errorf("invalid filename format: %s", filename)
	}

	if coord.Extension == "" {
		return coord, "", fmt.Errorf("missing extension: %s", filename)
	}

	return coord, checksumType, nil
}

// findSnapshotExtensionIndex finds where the extension starts in a snapshot filename remainder.
// Input format: "YYYYMMDD.HHMMSS-buildNum[-classifier].ext" e.g., "20240118.123456-1.jar"
// Returns the index of the last segment starting with "." or "-" before the extension.
func findSnapshotExtensionIndex(s string) int {
	// Pattern: timestamp.time-buildnum[-classifier].ext
	// We need to find where the artifact naming ends and extension begins
	// Look for pattern like "-1.jar" or "-1-sources.jar"
	parts := strings.Split(s, "-")
	if len(parts) < 2 {
		return -1
	}
	// The build number part contains the extension: "1.jar" or we have classifier after
	// Find the first part after timestamp that contains a dot for extension
	for i := 1; i < len(parts); i++ {
		if dotIdx := strings.Index(parts[i], "."); dotIdx > 0 {
			// Found extension, calculate position
			pos := 0
			for j := 0; j < i; j++ {
				pos += len(parts[j]) + 1 // +1 for the "-"
			}
			return pos + dotIdx
		}
	}
	return -1
}

// computeChecksum computes a checksum of the given data.
func computeChecksum(data []byte, checksumType string) string {
	var h hash.Hash
	switch checksumType {
	case ChecksumMD5:
		h = md5.New()
	case ChecksumSHA1:
		h = sha1.New()
	case ChecksumSHA256:
		h = sha256.New()
	case ChecksumSHA512:
		h = sha512.New()
	default:
		return ""
	}
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// contentTypeForExtension returns the content type for a file extension.
func contentTypeForExtension(ext string) string {
	switch ext {
	case ExtensionJAR, ExtensionWAR, ExtensionEAR, ExtensionAAR:
		return "application/java-archive"
	case ExtensionPOM:
		return "application/xml"
	case ExtensionZIP:
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
