package goproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/buildkite/content-cache/telemetry"
)

const (
	// DefaultSumDBName is the default checksum database name.
	DefaultSumDBName = "sum.golang.org"

	// DefaultSumDBURL is the default upstream sumdb URL.
	DefaultSumDBURL = "https://sum.golang.org"

	// maxSumdbResponseSize is the maximum size of a sumdb response to read into memory.
	// Tiles and lookups are small (typically < 100KB), but we set a reasonable limit.
	maxSumdbResponseSize = 5 << 20 // 5MB
)

// SumdbHandler implements the sumdb proxy protocol as an HTTP handler.
// It proxies requests to the upstream checksum database and caches tile/lookup responses.
type SumdbHandler struct {
	sumdbName string
	upstream  *SumdbUpstream
	index     *SumdbIndex
	store     store.Store
	logger    *slog.Logger
}

// SumdbHandlerOption configures a SumdbHandler.
type SumdbHandlerOption func(*SumdbHandler)

// WithSumdbName sets the allowed sumdb name.
func WithSumdbName(name string) SumdbHandlerOption {
	return func(h *SumdbHandler) {
		h.sumdbName = name
	}
}

// WithSumdbUpstream sets the upstream sumdb client.
func WithSumdbUpstream(upstream *SumdbUpstream) SumdbHandlerOption {
	return func(h *SumdbHandler) {
		h.upstream = upstream
	}
}

// WithSumdbLogger sets the logger for the handler.
func WithSumdbLogger(logger *slog.Logger) SumdbHandlerOption {
	return func(h *SumdbHandler) {
		h.logger = logger
	}
}

// NewSumdbHandler creates a new sumdb proxy handler.
func NewSumdbHandler(index *SumdbIndex, store store.Store, opts ...SumdbHandlerOption) *SumdbHandler {
	h := &SumdbHandler{
		sumdbName: DefaultSumDBName,
		upstream:  NewSumdbUpstream(),
		index:     index,
		store:     store,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *SumdbHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /sumdb/{sumdb-name}/{endpoint...}
	path := strings.TrimPrefix(r.URL.Path, "/sumdb/")
	if path == r.URL.Path {
		// No /sumdb/ prefix found
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Extract sumdb name and endpoint
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing sumdb name", http.StatusBadRequest)
		return
	}

	sumdbName := parts[0]
	endpoint := ""
	if len(parts) > 1 {
		endpoint = parts[1]
	}

	// Validate sumdb name
	if sumdbName != h.sumdbName {
		http.NotFound(w, r)
		return
	}

	logger := h.logger.With("sumdb", sumdbName, "endpoint", endpoint)

	// Route to appropriate handler
	switch {
	case endpoint == "supported":
		h.handleSupported(w, r)
	case endpoint == "latest":
		h.handleLatest(w, r, logger)
	case strings.HasPrefix(endpoint, "lookup/"):
		h.handleLookup(w, r, endpoint, logger)
	case strings.HasPrefix(endpoint, "tile/"):
		h.handleTile(w, r, endpoint, logger)
	default:
		http.Error(w, "invalid endpoint", http.StatusBadRequest)
	}
}

// handleSupported handles /sumdb/{name}/supported requests.
// Returns 200 OK to indicate this proxy supports sumdb proxying.
func (h *SumdbHandler) handleSupported(w http.ResponseWriter, r *http.Request) {
	telemetry.SetEndpoint(r, "supported")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)
	w.WriteHeader(http.StatusOK)
}

// handleLatest handles /sumdb/{name}/latest requests.
// Always proxied to upstream, never cached.
func (h *SumdbHandler) handleLatest(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	telemetry.SetEndpoint(r, "latest")
	telemetry.SetCacheResult(r, telemetry.CacheBypass)
	ctx := r.Context()

	logger.Debug("fetching latest from upstream")

	data, contentType, statusCode, err := h.upstream.Fetch(ctx, "latest")
	if err != nil {
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	h.writeResponse(w, data, contentType, statusCode)
}

// handleLookup handles /sumdb/{name}/lookup/{module}@{version} requests.
// Cached on 200 responses only.
func (h *SumdbHandler) handleLookup(w http.ResponseWriter, r *http.Request, endpoint string, logger *slog.Logger) {
	telemetry.SetEndpoint(r, "lookup")
	ctx := r.Context()

	// Try cache first
	data, err := h.index.Get(ctx, endpoint)
	if err == nil {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeResponse(w, data, "text/plain; charset=utf-8", http.StatusOK)
		return
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	data, contentType, statusCode, err := h.upstream.Fetch(ctx, endpoint)
	if err != nil {
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cache only 200 responses
	if statusCode == http.StatusOK {
		if err := h.index.Put(ctx, endpoint, data); err != nil {
			logger.Error("failed to cache lookup", "error", err)
		}
	}

	h.writeResponse(w, data, contentType, statusCode)
}

// handleTile handles /sumdb/{name}/tile/{H}/{L}/{K}[.p/{W}] requests.
// Cached on 200 responses only.
func (h *SumdbHandler) handleTile(w http.ResponseWriter, r *http.Request, endpoint string, logger *slog.Logger) {
	telemetry.SetEndpoint(r, "tile")
	ctx := r.Context()

	// Try cache first
	data, err := h.index.Get(ctx, endpoint)
	if err == nil {
		logger.Debug("cache hit")
		telemetry.SetCacheResult(r, telemetry.CacheHit)
		h.writeResponse(w, data, "application/octet-stream", http.StatusOK)
		return
	}

	// Fetch from upstream
	logger.Debug("cache miss, fetching from upstream")
	telemetry.SetCacheResult(r, telemetry.CacheMiss)

	data, contentType, statusCode, err := h.upstream.Fetch(ctx, endpoint)
	if err != nil {
		logger.Error("upstream fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cache only 200 responses
	if statusCode == http.StatusOK {
		if err := h.index.Put(ctx, endpoint, data); err != nil {
			logger.Error("failed to cache tile", "error", err)
		}
	}

	h.writeResponse(w, data, contentType, statusCode)
}

// writeResponse writes the response with appropriate headers.
func (h *SumdbHandler) writeResponse(w http.ResponseWriter, data []byte, contentType string, statusCode int) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(data) //nolint:gosec // transparent proxy forwarding cached sumdb data to client
}

// SumdbUpstream fetches data from an upstream sumdb.
type SumdbUpstream struct {
	baseURL string
	client  *http.Client
}

// SumdbUpstreamOption configures a SumdbUpstream.
type SumdbUpstreamOption func(*SumdbUpstream)

// WithSumdbUpstreamURL sets the upstream sumdb URL.
func WithSumdbUpstreamURL(url string) SumdbUpstreamOption {
	return func(u *SumdbUpstream) {
		u.baseURL = url
	}
}

// WithSumdbHTTPClient sets a custom HTTP client for the sumdb upstream.
func WithSumdbHTTPClient(client *http.Client) SumdbUpstreamOption {
	return func(u *SumdbUpstream) {
		u.client = client
	}
}

// NewSumdbUpstream creates a new upstream sumdb client.
func NewSumdbUpstream(opts ...SumdbUpstreamOption) *SumdbUpstream {
	u := &SumdbUpstream{
		baseURL: DefaultSumDBURL,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// Fetch fetches an endpoint from the upstream sumdb.
// Returns the response body, content-type, status code, and any error.
func (u *SumdbUpstream) Fetch(ctx context.Context, endpoint string) ([]byte, string, int, error) {
	url := fmt.Sprintf("%s/%s", u.baseURL, endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, "", 0, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, "", 0, fmt.Errorf("performing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response with size limit
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSumdbResponseSize))
	if err != nil {
		return nil, "", 0, fmt.Errorf("reading response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}

	return data, contentType, resp.StatusCode, nil
}

// SumdbIndex stores cached sumdb responses.
type SumdbIndex struct {
	envelope *metadb.EnvelopeIndex
}

// NewSumdbIndex creates a new sumdb index.
func NewSumdbIndex(envelope *metadb.EnvelopeIndex) *SumdbIndex {
	return &SumdbIndex{envelope: envelope}
}

// Get retrieves a cached response for the given endpoint.
func (i *SumdbIndex) Get(ctx context.Context, endpoint string) ([]byte, error) {
	return i.envelope.Get(ctx, endpoint)
}

// Put stores a response for the given endpoint.
func (i *SumdbIndex) Put(ctx context.Context, endpoint string, data []byte) error {
	return i.envelope.Put(ctx, endpoint, data, metadb.ContentType_CONTENT_TYPE_OCTET_STREAM, nil)
}
