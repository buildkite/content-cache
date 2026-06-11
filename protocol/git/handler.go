package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/download"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/telemetry"
	"github.com/klauspost/compress/gzip"
)

// Handler implements the Git Smart HTTP proxy as an HTTP handler.
type Handler struct {
	index                   *Index
	store                   store.Store
	router                  *Router
	logger                  *slog.Logger
	downloader              *download.Downloader
	allowedHosts            map[string]bool
	maxRequestBodySize      int64
	maxDecompressedBodySize int64
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithLogger sets the logger for the handler.
func WithLogger(logger *slog.Logger) HandlerOption {
	return func(h *Handler) {
		h.logger = logger
	}
}

// WithUpstream sets a single upstream Git client (for backward compatibility).
// This creates a router with the given upstream as the fallback.
func WithUpstream(upstream *Upstream) HandlerOption {
	return func(h *Handler) {
		r, _ := NewRouter(nil, WithFallback(upstream))
		h.router = r
	}
}

// WithRouter sets the Git router for prefix-based upstream selection.
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

// WithAllowedHosts sets the allowlist of permitted upstream Git hosts.
func WithAllowedHosts(hosts []string) HandlerOption {
	return func(h *Handler) {
		h.allowedHosts = make(map[string]bool, len(hosts))
		for _, host := range hosts {
			h.allowedHosts[host] = true
		}
	}
}

// WithMaxRequestBodySize sets the maximum size for upload-pack request bodies.
func WithMaxRequestBodySize(size int64) HandlerOption {
	return func(h *Handler) {
		h.maxRequestBodySize = size
	}
}

// WithMaxDecompressedBodySize sets the maximum allowed decompressed size for
// gzip-encoded upload-pack request bodies.
func WithMaxDecompressedBodySize(size int64) HandlerOption {
	return func(h *Handler) {
		h.maxDecompressedBodySize = size
	}
}

// NewHandler creates a new Git proxy handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	defaultRouter, _ := NewRouter(nil, WithFallback(NewUpstream()))
	h := &Handler{
		index:                   index,
		store:                   store,
		router:                  defaultRouter,
		logger:                  slog.Default(),
		allowedHosts:            make(map[string]bool),
		maxRequestBodySize:      DefaultMaxRequestBodySize,
		maxDecompressedBodySize: DefaultMaxDecompressedBodySize,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// gitPathResult holds the parsed components of a Git proxy path.
type gitPathResult struct {
	Repo   RepoRef
	Action string // "info/refs" or "git-upload-pack" or "git-receive-pack"
}

// parseGitPath extracts the repository reference and action from a request path.
// The trailing ".git" on the repository is optional.
// Expected format: /{host}/{repoPath}[.git]/{action}
// Example: /github.com/user/repo.git/info/refs
// Example: /github.com/user/repo/info/refs
// Example: /gitlab.com/group/sub/repo.git/git-upload-pack
// Example: /gitlab.com/group/sub/repo/git-upload-pack
func parseGitPath(path string) (*gitPathResult, error) {
	// Remove leading slash
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}

	// Reject path traversal
	for seg := range strings.SplitSeq(path, "/") {
		if seg == ".." || seg == "" {
			return nil, fmt.Errorf("invalid path segment")
		}
	}

	// Identify the action from its known suffix. Everything before it is the
	// repository section, which may or may not carry a trailing ".git".
	var action, repoSection string
	switch {
	case strings.HasSuffix(path, "/info/refs"):
		action, repoSection = "info/refs", strings.TrimSuffix(path, "/info/refs")
	case strings.HasSuffix(path, "/git-upload-pack"):
		action, repoSection = "git-upload-pack", strings.TrimSuffix(path, "/git-upload-pack")
	case strings.HasSuffix(path, "/git-receive-pack"):
		action, repoSection = "git-receive-pack", strings.TrimSuffix(path, "/git-receive-pack")
	default:
		return nil, fmt.Errorf("unknown action")
	}

	// Trim the optional ".git" suffix from the repository section.
	repoSection = strings.TrimSuffix(repoSection, ".git")

	// Split into host and repo path
	host, repoPath, found := strings.Cut(repoSection, "/")
	if !found {
		return nil, fmt.Errorf("missing repository path")
	}

	if host == "" || repoPath == "" {
		return nil, fmt.Errorf("empty host or repository path")
	}

	return &gitPathResult{
		Repo:   RepoRef{Host: host, RepoPath: repoPath},
		Action: action,
	}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parsed, err := parseGitPath(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Validate host against allowlist
	if !h.allowedHosts[parsed.Repo.Host] {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	switch parsed.Action {
	case "info/refs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleInfoRefs(w, r, parsed.Repo)

	case "git-upload-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleUploadPack(w, r, parsed.Repo)

	case "git-receive-pack":
		http.Error(w, "push not allowed", http.StatusForbidden)

	default:
		http.NotFound(w, r)
	}
}

// handleInfoRefs proxies the info/refs discovery request to upstream.
// This is always a passthrough — never cached, since refs change.
func (h *Handler) handleInfoRefs(w http.ResponseWriter, r *http.Request, repo RepoRef) {
	ctx := r.Context()
	logger := h.logger.With("repo", repo.String(), "endpoint", "info/refs")

	telemetry.SetEndpoint(r, "info/refs")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)

	// Reject git-receive-pack (push discovery)
	if r.URL.Query().Get("service") == "git-receive-pack" {
		http.Error(w, "push not allowed", http.StatusForbidden)
		return
	}

	gitProtocol := r.Header.Get("Git-Protocol")

	upstream := h.router.Match(repo)
	rc, contentType, err := upstream.FetchInfoRefs(ctx, repo, gitProtocol)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		logger.Error("upstream info/refs failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", contentType)
	if _, err := io.Copy(w, rc); err != nil {
		logger.Error("failed to stream info/refs response", "error", err)
	}
}

// handleUploadPack handles git-upload-pack requests with pack-level caching.
func (h *Handler) handleUploadPack(w http.ResponseWriter, r *http.Request, repo RepoRef) {
	ctx := r.Context()
	logger := h.logger.With("repo", repo.String(), "endpoint", "upload-pack")

	telemetry.SetEndpoint(r, "upload-pack")

	// Stream the (size-limited) request body to a temp file to avoid large heap allocations.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBodySize)

	rawFile, err := os.CreateTemp("", "git-request-*")
	if err != nil {
		logger.Error("failed to create temp file for request body", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = os.Remove(rawFile.Name()) }()

	// activeBodyFile is the file that carries the (possibly decompressed) body
	// to fetchAndStorePack. Its ownership transfers there, which closes it.
	var activeBodyFile *os.File
	var bodyHash contentcache.Hash

	if r.Header.Get("Content-Encoding") == "gzip" {
		// Stream compressed body to rawFile, then decompress into a second temp file.
		if _, copyErr := io.Copy(rawFile, r.Body); copyErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(copyErr, &maxBytesErr) {
				_ = rawFile.Close()
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			_ = rawFile.Close()
			logger.Error("failed to stream compressed request body", "error", copyErr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if _, seekErr := rawFile.Seek(0, io.SeekStart); seekErr != nil {
			_ = rawFile.Close()
			logger.Error("failed to seek compressed request body temp file", "error", seekErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		gz, gzErr := gzip.NewReader(rawFile)
		if gzErr != nil {
			_ = rawFile.Close()
			logger.Error("failed to create gzip reader for request body", "error", gzErr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		decompFile, decompErr := os.CreateTemp("", "git-request-decomp-*")
		if decompErr != nil {
			_ = gz.Close()
			_ = rawFile.Close()
			logger.Error("failed to create temp file for decompressed body", "error", decompErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = os.Remove(decompFile.Name()) }()

		hr := contentcache.NewHashingReader(io.LimitReader(gz, h.maxDecompressedBodySize))
		decompSize, copyErr := io.Copy(decompFile, hr)
		_ = gz.Close()
		_ = rawFile.Close()
		if copyErr != nil {
			_ = decompFile.Close()
			logger.Error("failed to decompress gzip request body", "error", copyErr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if decompSize >= h.maxDecompressedBodySize {
			_ = decompFile.Close()
			logger.Error("decompressed request body exceeds size limit", "limit", h.maxDecompressedBodySize)
			http.Error(w, "decompressed request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		bodyHash = hr.Sum()
		logger.Debug("decompressed gzip request body", "decompressed_size", decompSize)

		if _, seekErr := decompFile.Seek(0, io.SeekStart); seekErr != nil {
			_ = decompFile.Close()
			logger.Error("failed to seek decompressed body temp file", "error", seekErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		activeBodyFile = decompFile
	} else {
		// Non-gzip: stream directly while computing hash.
		hr := contentcache.NewHashingReader(r.Body)
		if _, copyErr := io.Copy(rawFile, hr); copyErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(copyErr, &maxBytesErr) {
				_ = rawFile.Close()
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			_ = rawFile.Close()
			logger.Error("failed to stream request body", "error", copyErr)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		bodyHash = hr.Sum()

		if _, seekErr := rawFile.Seek(0, io.SeekStart); seekErr != nil {
			_ = rawFile.Close()
			logger.Error("failed to seek request body temp file", "error", seekErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		activeBodyFile = rawFile
	}

	gitProtocol := r.Header.Get("Git-Protocol")
	cacheKey := fmt.Sprintf("%s:%s:%s", repo.String(), gitProtocol, bodyHash.String())

	logger.Debug("upload-pack request details",
		"body_hash", bodyHash.ShortString(),
		"git_protocol", gitProtocol,
		"cache_key", cacheKey,
	)
	if logger.Enabled(ctx, slog.LevelDebug) {
		const debugReadLimit = 4 * 1024 * 1024
		if _, seekErr := activeBodyFile.Seek(0, io.SeekStart); seekErr == nil {
			debugBytes, _ := io.ReadAll(io.LimitReader(activeBodyFile, debugReadLimit))
			_, _ = activeBodyFile.Seek(0, io.SeekStart)
			logPktLineSummary(ctx, logger, debugBytes)
		}
	}

	// In the git v2 wire protocol `ls-refs` is passed to the git-upload-pack endpoint.
	// This type of request cannot be cached, as it is backed by mutable refs upstream.
	isLsRefs, lsRefsErr := isLsRefs(activeBodyFile)
	if lsRefsErr != nil {
		logger.Error("failed to check if git command includes ls-refs", "error", lsRefsErr)
	} else if isLsRefs {
		telemetry.SetCacheResult(r, telemetry.CacheBypass)

		upstream := h.router.Match(repo)
		rc, err := upstream.FetchUploadPack(ctx, repo, gitProtocol, activeBodyFile)
		_ = activeBodyFile.Close()
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		if err != nil {
			logger.Error("upstream git-upload-pack ls-refs failed", "error", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer func() { _ = rc.Close() }()

		w.Header().Set("Content-Type", ContentTypeUploadPackResult)
		if _, err := io.Copy(w, rc); err != nil {
			logger.Error("failed to stream git-upload-pack ls-refs response", "error", err)
		}
		return
	}

	// Check cache
	cached, err := h.index.GetCachedPack(ctx, cacheKey)
	if err == nil {
		// Verify the blob still exists in the store before serving
		exists, storeErr := h.store.Has(ctx, cached.ResponseHash)
		if storeErr != nil {
			logger.Error("cache store check failed", "hash", cached.ResponseHash.ShortString(), "error", storeErr)
		}
		if exists {
			upstream := h.router.Match(repo)
			if err := upstream.checkAuth(ctx, repo); err != nil {
				logger.Error("upstream auth failed for cached pack", "error", err)
				_ = activeBodyFile.Close()
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}

			logger.Debug("cache hit", "hash", cached.ResponseHash.ShortString())
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			_ = activeBodyFile.Close()

			download.ServeFromStore(ctx, w, r, h.store, &download.Result{
				Hash: cached.ResponseHash,
				Size: cached.ResponseSize,
			}, download.ServeOptions{ContentType: ContentTypeUploadPackResult}, logger)
			return
		}
		// Blob missing from store — evict stale index entry and fall through to upstream
		logger.Warn("cache index references missing blob, evicting stale entry",
			"hash", cached.ResponseHash.ShortString(),
			"cache_key", cacheKey,
		)
		if delErr := h.index.DeleteCachedPack(ctx, cacheKey); delErr != nil {
			logger.Error("failed to evict stale cache entry", "error", delErr)
		}
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		logger.Error("cache lookup failed", "error", err)
	}

	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	sfKey := fmt.Sprintf("git:upload-pack:%s", cacheKey)

	// Transfer ownership of activeBodyFile to fetchAndStorePack via the closure.
	// If singleflight deduplicates this call (our closure is never invoked), we must
	// close bodyFile ourselves so the file descriptor is not leaked.
	bodyFile := activeBodyFile
	closureRan := false
	result, _, err := h.downloader.Do(ctx, sfKey, func(dlCtx context.Context) (*download.Result, error) {
		closureRan = true
		return h.fetchAndStorePack(dlCtx, repo, gitProtocol, bodyFile, bodyHash, cacheKey, logger)
	})
	if !closureRan {
		_ = bodyFile.Close()
	}

	download.HandleResult(download.HandleResultParams{
		Writer:     w,
		Request:    r,
		Downloader: h.downloader,
		Key:        sfKey,
		Result:     result,
		Err:        err,
		Store:      h.store,
		IsNotFound: func(e error) bool { return errors.Is(e, ErrNotFound) },
		Opts:       download.ServeOptions{ContentType: ContentTypeUploadPackResult},
		Logger:     logger,
	})
}

// fetchAndStorePack fetches an upload-pack response from upstream, stores it in CAFS,
// and updates the index. It takes ownership of bodyFile and closes it before returning.
func (h *Handler) fetchAndStorePack(ctx context.Context, repo RepoRef, gitProtocol string, bodyFile io.ReadSeeker, bodyHash contentcache.Hash, cacheKey string, logger *slog.Logger) (*download.Result, error) {
	defer func() {
		if c, ok := bodyFile.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	logger.Debug("fetching upload-pack from upstream",
		"upstream_url", repo.UpstreamURL(),
		"git_protocol", gitProtocol,
	)

	upstream := h.router.Match(repo)
	rc, err := upstream.FetchUploadPack(ctx, repo, gitProtocol, bodyFile)
	if err != nil {
		logger.Error("download failed", "error", err)
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	// Stream response to temp file while computing hash
	tmpFile, err := os.CreateTemp("", "git-pack-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hr := contentcache.NewHashingReader(rc)

	size, err := io.Copy(tmpFile, hr)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("reading pack data: %w", err)
	}
	hash := hr.Sum()

	// Seek to beginning for CAFS storage
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	// Store in CAFS
	storedHash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("storing pack: %w", err)
	}
	_ = tmpFile.Close()

	if storedHash != hash {
		logger.Warn("hash mismatch during storage", "expected", hash.ShortString(), "got", storedHash.ShortString())
	}

	// Update index
	cached := &CachedPack{
		RequestHash:  bodyHash,
		ResponseHash: hash,
		ResponseSize: size,
		Repo:         repo.String(),
		GitProtocol:  gitProtocol,
		CachedAt:     time.Now(),
	}
	if err := h.index.PutCachedPack(ctx, cacheKey, cached); err != nil {
		logger.Error("failed to update index", "error", err)
	} else {
		logger.Info("cached pack", "hash", hash.ShortString(), "size", size)
	}

	return &download.Result{Hash: hash, Size: size}, nil
}

// logPktLineSummary logs a summary of the git pkt-line request body for debugging.
// It parses pkt-line framing (4-hex-digit length prefix) and summarizes the content.
func logPktLineSummary(ctx context.Context, logger *slog.Logger, body []byte) {
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}

	var (
		wantCount, haveCount, shallowCount int
		hasDone, hasFilter                 bool
		command                            string
		firstLines                         []string
	)

	pos := 0
	lineNum := 0
	for pos < len(body) {
		if pos+4 > len(body) {
			break
		}

		// Parse 4-hex-digit length prefix
		hexLen := string(body[pos : pos+4])

		// Flush packet (0000) or delimiter (0001)
		if hexLen == "0000" || hexLen == "0001" {
			if lineNum < 10 {
				firstLines = append(firstLines, hexLen)
			}
			pos += 4
			lineNum++
			continue
		}

		pktLen, err := parseHexLen(hexLen)
		if err != nil || pktLen < 4 {
			// Not valid pkt-line, log what we can and stop
			logger.Debug("pkt-line parse error", "pos", pos, "hex", hexLen, "error", err)
			break
		}

		if pos+pktLen > len(body) {
			logger.Debug("pkt-line truncated", "pos", pos, "pkt_len", pktLen, "remaining", len(body)-pos)
			break
		}

		// Content is everything after the 4-byte length prefix, trimmed
		content := strings.TrimRight(string(body[pos+4:pos+pktLen]), "\n")

		// Log first few lines verbatim for debugging
		if lineNum < 10 {
			firstLines = append(firstLines, content)
		}

		switch {
		case strings.HasPrefix(content, "command="):
			command = strings.TrimPrefix(content, "command=")
		case strings.HasPrefix(content, "want "):
			wantCount++
		case strings.HasPrefix(content, "have "):
			haveCount++
		case strings.HasPrefix(content, "shallow "):
			shallowCount++
		case content == "done":
			hasDone = true
		case strings.HasPrefix(content, "filter "):
			hasFilter = true
		}

		pos += pktLen
		lineNum++
	}

	logger.Debug("pkt-line summary",
		"command", command,
		"wants", wantCount,
		"haves", haveCount,
		"shallows", shallowCount,
		"done", hasDone,
		"filter", hasFilter,
		"total_lines", lineNum,
		"first_lines", firstLines,
	)
}

// parseHexLen parses a 4-character hex string into an integer.
func parseHexLen(s string) (int, error) {
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid hex length %q: %w", s, err)
	}
	return int(n), nil
}

// isLsRefs determines if a particular request is a ls-refs command.
//
// The output of ls-refs commands change as the upstream updates,
// and so they cannot be cached in the same way a normal git-upload-pack request can be.
func isLsRefs(activeBody io.ReadSeeker) (bool, error) {
	if _, err := activeBody.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	defer func() { _, _ = activeBody.Seek(0, io.SeekStart) }()

	body, err := io.ReadAll(io.LimitReader(activeBody, maxLsRefsScanSize))
	if err != nil {
		return false, err
	}

	pos := 0
	for pos+4 <= len(body) {
		pktLen, err := parseHexLen(string(body[pos : pos+4]))
		if err != nil {
			return false, err
		}
		if pktLen == 0 || pktLen == 1 {
			// pktLen == 0 -> flush packet
			// pktLen == 1 -> delimiter packet
			// in both cases, they are just special control packets
			pos += 4
			continue
		}
		if pktLen < 4 {
			return false, fmt.Errorf("git protocol malformed packet size")
		}
		if pos+pktLen > len(body) {
			break
		}
		content := strings.TrimRight(string(body[pos+4:pos+pktLen]), "\n")
		cmd, ok := strings.CutPrefix(content, "command=")
		if !ok {
			pos += pktLen
			continue
		}
		return cmd == "ls-refs", nil
	}

	return false, nil
}
