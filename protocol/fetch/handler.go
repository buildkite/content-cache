package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/download"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/buildkite/content-cache/telemetry"
)

const defaultTimeout = 10 * time.Minute

var (
	errHostNotAllowed = errors.New("host not allowed")

	defaultGitHubReleaseRedirectHosts = []string{
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"github-releases.githubusercontent.com",
	}
)

// Handler serves cached direct-download artefacts.
type Handler struct {
	index      *Index
	store      store.Store
	client     *http.Client
	logger     *slog.Logger
	downloader *download.Downloader

	allowedHosts               hostSet
	githubReleaseHost          string
	githubReleaseRedirectHosts hostSet
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithLogger sets the logger for the handler.
func WithLogger(logger *slog.Logger) HandlerOption {
	return func(h *Handler) {
		h.logger = logger
	}
}

// WithHTTPClient sets the HTTP client used for upstream fetches.
func WithHTTPClient(client *http.Client) HandlerOption {
	return func(h *Handler) {
		h.client = client
	}
}

// WithDownloader sets the shared singleflight downloader.
func WithDownloader(dl *download.Downloader) HandlerOption {
	return func(h *Handler) {
		h.downloader = dl
	}
}

// WithAllowedHosts sets the allowlist for the generic /fetch route.
func WithAllowedHosts(hosts []string) HandlerOption {
	return func(h *Handler) {
		h.allowedHosts = newHostSet(hosts)
	}
}

// WithGitHubReleaseHost overrides the host used by the /github-release route.
// This is mainly useful for tests.
func WithGitHubReleaseHost(host string) HandlerOption {
	return func(h *Handler) {
		h.githubReleaseHost = host
	}
}

// WithGitHubReleaseRedirectHosts overrides the allowed redirect hosts for /github-release.
// This is mainly useful for tests.
func WithGitHubReleaseRedirectHosts(hosts ...string) HandlerOption {
	return func(h *Handler) {
		h.githubReleaseRedirectHosts = newHostSet(hosts)
	}
}

// NewHandler creates a new fetch handler.
func NewHandler(index *Index, store store.Store, opts ...HandlerOption) *Handler {
	h := &Handler{
		index:      index,
		store:      store,
		client:     &http.Client{Timeout: defaultTimeout},
		logger:     slog.Default(),
		downloader: download.New(),

		allowedHosts:               newHostSet(nil),
		githubReleaseHost:          "github.com",
		githubReleaseRedirectHosts: newHostSet(defaultGitHubReleaseRedirectHosts),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles generic /fetch/{host}/{path...} requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host, upstreamPath, err := parseFetchPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.allowedHosts.Contains(host) {
		http.Error(w, errHostNotAllowed.Error(), http.StatusForbidden)
		return
	}

	upstream := &url.URL{Scheme: "https", Host: canonicalHTTPSAuthority(host), Path: upstreamPath, RawQuery: r.URL.RawQuery}
	h.serveUpstream(w, r, upstream, h.allowedHosts, "resource")
}

// ServeGitHubRelease handles /github-release/{owner}/{repo}/releases/download/... requests.
func (h *Handler) ServeGitHubRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	upstreamPath, err := parseGitHubReleasePath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	allowed := newHostSet(nil)
	allowed.Add(h.githubReleaseHost)
	allowed.Merge(h.githubReleaseRedirectHosts)

	upstream := &url.URL{Scheme: "https", Host: canonicalHTTPSAuthority(h.githubReleaseHost), Path: upstreamPath, RawQuery: r.URL.RawQuery}
	h.serveUpstream(w, r, upstream, allowed, "github_release")
}

func (h *Handler) serveUpstream(w http.ResponseWriter, r *http.Request, upstream *url.URL, allowedHosts hostSet, endpoint string) {
	telemetry.SetEndpoint(r, endpoint)
	ctx := r.Context()
	cacheKey := upstream.String()
	logger := h.logger.With("upstream_url", cacheKey, "endpoint", endpoint)
	if r.Header.Get("Range") != "" {
		telemetry.SetCacheResult(r, telemetry.CacheBypass)
		writeRangeNotSupported(w)
		return
	}

	if entry, err := h.index.Get(ctx, cacheKey); err == nil {
		if served, serveErr := h.serveCached(w, r, entry, logger); serveErr == nil && served {
			telemetry.SetCacheResult(r, telemetry.CacheHit)
			return
		} else if serveErr != nil {
			logger.Error("failed to serve cached resource", "error", serveErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else if !errors.Is(err, ErrNotFound) {
		logger.Error("cache lookup failed", "error", err)
	}

	telemetry.SetCacheResult(r, telemetry.CacheMiss)
	if isConditionalRequest(r) {
		h.proxyConditionalMiss(w, r, upstream, allowedHosts, logger)
		return
	}
	if r.Method == http.MethodHead {
		h.handleHeadMiss(w, r, upstream, allowedHosts, logger)
		return
	}

	if err := h.ensureCached(ctx, cacheKey, allowedHosts); err != nil {
		h.writeFetchError(w, r, logger, err)
		return
	}

	entry, err := h.index.Get(ctx, cacheKey)
	if err != nil {
		logger.Error("cache lookup after download failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, err := h.serveCached(w, r, entry, logger); err != nil {
		logger.Error("failed to serve freshly cached resource", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *Handler) handleHeadMiss(w http.ResponseWriter, r *http.Request, upstream *url.URL, allowedHosts hostSet, logger *slog.Logger) {
	resp, err := h.doRequest(r.Context(), http.MethodHead, upstream.String(), allowedHosts, r)
	if err != nil {
		h.writeFetchError(w, r, logger, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.copyHeaders(w, resp.Header, 0)
	if resp.StatusCode == http.StatusNotModified {
		writeNotModified(w, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) proxyConditionalMiss(w http.ResponseWriter, r *http.Request, upstream *url.URL, allowedHosts hostSet, logger *slog.Logger) {
	resp, err := h.doRequest(r.Context(), r.Method, upstream.String(), allowedHosts, r)
	if err != nil {
		h.writeFetchError(w, r, logger, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		writeNotModified(w, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
		return
	}

	if r.Method == http.MethodHead {
		h.copyHeaders(w, resp.Header, 0)
		w.WriteHeader(http.StatusOK)
		return
	}

	if _, err := h.cacheResponse(r.Context(), upstream.String(), resp); err != nil {
		logger.Error("failed to cache conditional upstream response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	entry, err := h.index.Get(r.Context(), upstream.String())
	if err != nil {
		logger.Error("cache lookup after conditional download failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, err := h.serveCached(w, r, entry, logger); err != nil {
		logger.Error("failed to serve freshly cached conditional resource", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *Handler) ensureCached(ctx context.Context, upstreamURL string, allowedHosts hostSet) error {
	_, _, err := h.downloader.Do(ctx, upstreamURL, func(ctx context.Context) (*download.Result, error) {
		return h.downloadAndCache(ctx, upstreamURL, allowedHosts)
	})
	if err != nil {
		forgetDownloadOnError(h.downloader, upstreamURL, err)
		return err
	}
	return nil
}

func (h *Handler) downloadAndCache(ctx context.Context, upstreamURL string, allowedHosts hostSet) (*download.Result, error) {
	resp, err := h.doRequest(ctx, http.MethodGet, upstreamURL, allowedHosts, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return h.cacheResponse(ctx, upstreamURL, resp)
}

func (h *Handler) cacheResponse(ctx context.Context, upstreamURL string, resp *http.Response) (*download.Result, error) {

	tmpFile, err := os.CreateTemp("", "content-cache-fetch-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	size, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("downloading upstream body: %w", err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewinding temp file for storage: %w", err)
	}

	hash, err := h.store.Put(ctx, tmpFile)
	if err != nil {
		return nil, fmt.Errorf("storing blob: %w", err)
	}

	entry := &CachedResource{
		UpstreamURL:        upstreamURL,
		BlobHash:           contentcache.NewBlobRef(hash).String(),
		Size:               size,
		ContentType:        contentTypeOrDefault(resp.Header.Get("Content-Type")),
		ContentEncoding:    resp.Header.Get("Content-Encoding"),
		ContentDisposition: resp.Header.Get("Content-Disposition"),
		ETag:               resp.Header.Get("ETag"),
		LastModified:       resp.Header.Get("Last-Modified"),
		CachedAt:           time.Now(),
	}

	opts := metadb.PutOptions{
		Etag:     entry.ETag,
		Upstream: resp.Request.URL.Hostname(),
	}
	if entry.LastModified != "" {
		if lastModified, err := http.ParseTime(entry.LastModified); err == nil {
			opts.LastModified = lastModified
		}
	}

	if err := h.index.Put(ctx, upstreamURL, entry, opts); err != nil {
		return nil, fmt.Errorf("storing cache metadata: %w", err)
	}

	return &download.Result{Hash: hash, Size: size}, nil
}

func (h *Handler) serveCached(w http.ResponseWriter, r *http.Request, entry *CachedResource, logger *slog.Logger) (bool, error) {
	if isNotModified(r, entry) {
		writeNotModified(w, entry.ETag, entry.LastModified)
		return true, nil
	}

	ref, err := contentcache.ParseBlobRef(entry.BlobHash)
	if err != nil {
		logger.Warn("invalid blob ref in cache entry", "blob_hash", entry.BlobHash, "error", err)
		_ = h.index.Delete(r.Context(), entry.UpstreamURL)
		return false, nil
	}

	rc, err := h.store.Get(r.Context(), ref.Hash)
	if err != nil {
		logger.Warn("cached blob missing from store", "blob_hash", entry.BlobHash, "error", err)
		_ = h.index.Delete(r.Context(), entry.UpstreamURL)
		return false, nil
	}
	defer func() { _ = rc.Close() }()

	h.copyCachedHeaders(w, entry)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	if _, err := io.Copy(w, rc); err != nil {
		return true, fmt.Errorf("streaming cached blob: %w", err)
	}
	return true, nil
}

func (h *Handler) doRequest(ctx context.Context, method, upstreamURL string, allowedHosts hostSet, source *http.Request) (*http.Response, error) {
	client := h.clientWithRedirectPolicy(allowedHosts)
	req, err := http.NewRequestWithContext(ctx, method, upstreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating upstream request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	copyConditionalRequestHeaders(req, source)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected upstream status %d", resp.StatusCode)
	}
	return resp, nil
}

func (h *Handler) clientWithRedirectPolicy(allowedHosts hostSet) *http.Client {
	client := *h.client
	baseRedirect := h.client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: %s", errHostNotAllowed, req.URL.String())
		}
		if !allowedHosts.Contains(req.URL.Host) {
			return fmt.Errorf("%w: %s", errHostNotAllowed, req.URL.Host)
		}
		if baseRedirect != nil {
			return baseRedirect(req, via)
		}
		return nil
	}
	return &client
}

func (h *Handler) writeFetchError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errHostNotAllowed):
		http.Error(w, errHostNotAllowed.Error(), http.StatusForbidden)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "request timeout", http.StatusGatewayTimeout)
	default:
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

func copyConditionalRequestHeaders(dst *http.Request, src *http.Request) {
	if src == nil {
		return
	}
	if etag := src.Header.Get("If-None-Match"); etag != "" {
		dst.Header.Set("If-None-Match", etag)
	}
	if modifiedSince := src.Header.Get("If-Modified-Since"); modifiedSince != "" {
		dst.Header.Set("If-Modified-Since", modifiedSince)
	}
}

func (h *Handler) copyCachedHeaders(w http.ResponseWriter, entry *CachedResource) {
	if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	}
	if entry.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", entry.ContentEncoding)
	}
	if entry.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", entry.ContentDisposition)
	}
	if entry.ETag != "" {
		w.Header().Set("ETag", entry.ETag)
	}
	if entry.LastModified != "" {
		w.Header().Set("Last-Modified", entry.LastModified)
	}
	if entry.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
	}
}

func (h *Handler) copyHeaders(w http.ResponseWriter, src http.Header, size int64) {
	if contentType := src.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentTypeOrDefault(contentType))
	} else {
		w.Header().Set("Content-Type", contentTypeOrDefault(""))
	}
	if contentEncoding := src.Get("Content-Encoding"); contentEncoding != "" {
		w.Header().Set("Content-Encoding", contentEncoding)
	}
	if contentDisposition := src.Get("Content-Disposition"); contentDisposition != "" {
		w.Header().Set("Content-Disposition", contentDisposition)
	}
	if etag := src.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lastModified := src.Get("Last-Modified"); lastModified != "" {
		w.Header().Set("Last-Modified", lastModified)
	}
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	} else if contentLength := src.Get("Content-Length"); contentLength != "" {
		w.Header().Set("Content-Length", contentLength)
	}
}

func parseFetchPath(requestPath string) (host string, upstreamPath string, err error) {
	trimmed := strings.TrimPrefix(requestPath, "/")
	host, remainder, ok := strings.Cut(trimmed, "/")
	if !ok || host == "" || remainder == "" {
		return "", "", fmt.Errorf("invalid fetch path")
	}
	return host, "/" + remainder, nil
}

func parseGitHubReleasePath(requestPath string) (string, error) {
	trimmed := strings.TrimPrefix(requestPath, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 6 || parts[2] != "releases" || parts[3] != "download" {
		return "", fmt.Errorf("invalid github release path")
	}
	return "/" + trimmed, nil
}

func contentTypeOrDefault(contentType string) string {
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

func isConditionalRequest(r *http.Request) bool {
	return r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != ""
}

func isNotModified(r *http.Request, entry *CachedResource) bool {
	if etag := r.Header.Get("If-None-Match"); etag != "" {
		return matchesIfNoneMatch(etag, entry.ETag)
	}
	if modifiedSince := r.Header.Get("If-Modified-Since"); modifiedSince != "" && entry.LastModified != "" {
		parsedModifiedSince, err := http.ParseTime(modifiedSince)
		if err != nil {
			return false
		}
		lastModified, err := http.ParseTime(entry.LastModified)
		if err != nil {
			return false
		}
		return !lastModified.After(parsedModifiedSince)
	}
	return false
}

func matchesIfNoneMatch(header, etag string) bool {
	want := weakETag(etag)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if etag != "" && weakETag(candidate) == want {
			return true
		}
	}
	return false
}

func weakETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	return strings.TrimPrefix(etag, "w/")
}

func writeNotModified(w http.ResponseWriter, etag, lastModified string) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lastModified != "" {
		w.Header().Set("Last-Modified", lastModified)
	}
	w.WriteHeader(http.StatusNotModified)
}

func writeRangeNotSupported(w http.ResponseWriter) {
	w.Header().Set("Accept-Ranges", "none")
	http.Error(w, "range requests are not supported", http.StatusNotImplemented)
}

func forgetDownloadOnError(d *download.Downloader, key string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	d.Forget(key)
}

type hostSet struct {
	hosts       map[string]struct{}
	authorities map[string]struct{}
}

func newHostSet(hosts []string) hostSet {
	set := hostSet{
		hosts:       make(map[string]struct{}, len(hosts)),
		authorities: make(map[string]struct{}, len(hosts)),
	}
	for _, host := range hosts {
		set.Add(host)
	}
	return set
}

func (s hostSet) Add(host string) {
	hostname, authority, port := normalizeHost(host)
	if hostname == "" {
		return
	}
	if port == "" || port == "443" {
		s.hosts[hostname] = struct{}{}
		return
	}
	s.authorities[authority] = struct{}{}
}

func (s hostSet) Contains(host string) bool {
	hostname, authority, port := normalizeHost(host)
	if hostname == "" {
		return false
	}
	if port == "" || port == "443" {
		_, ok := s.hosts[hostname]
		return ok
	}
	_, ok := s.authorities[authority]
	return ok
}

func (s hostSet) Merge(other hostSet) {
	for host := range other.hosts {
		s.hosts[host] = struct{}{}
	}
	for authority := range other.authorities {
		s.authorities[authority] = struct{}{}
	}
}

func normalizeHost(host string) (hostname string, authority string, port string) {
	if host == "" {
		return "", "", ""
	}
	if parsed, err := url.Parse("https://" + host); err == nil {
		hostname = strings.ToLower(parsed.Hostname())
		port = parsed.Port()
		if hostname == "" {
			return "", "", ""
		}
		if port != "" {
			return hostname, strings.ToLower(net.JoinHostPort(hostname, port)), port
		}
		return hostname, hostname, ""
	}
	hostname = strings.ToLower(host)
	return hostname, hostname, ""
}

func canonicalHTTPSAuthority(host string) string {
	hostname, authority, port := normalizeHost(host)
	if hostname == "" {
		return host
	}
	if port == "" || port == "443" {
		return hostname
	}
	return authority
}
