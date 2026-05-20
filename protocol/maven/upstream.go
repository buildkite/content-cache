package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTimeout is the default timeout for upstream requests.
	DefaultTimeout = 30 * time.Second

	// DefaultPerUpstreamTimeout caps a single upstream attempt when multiple
	// upstreams are configured, so a slow upstream doesn't block fallback.
	DefaultPerUpstreamTimeout = 5 * time.Second
)

// ErrNotFound is returned when an artifact is not found upstream.
var ErrNotFound = errors.New("artifact not found")

// Upstream fetches artifacts from one or more upstream Maven repositories.
// When multiple base URLs are configured, fetches try them in order and fall
// through on 404, with negative caching per (upstream, path) (see negcache.go)
// to avoid repeatedly probing upstreams that have already returned 404.
type Upstream struct {
	baseURLs       []string
	client         *http.Client
	userAgent      string
	perTryTimeout  time.Duration
	artifactNegTTL time.Duration
	metadataNegTTL time.Duration
	negCacheStore  NegativeCacheStore
}

// UpstreamOption configures an Upstream.
type UpstreamOption func(*Upstream)

// WithRepositoryURL sets a single upstream repository URL. Replaces any
// previously configured URLs.
func WithRepositoryURL(url string) UpstreamOption {
	return func(u *Upstream) {
		u.baseURLs = []string{normalizeUpstreamURL(url)}
	}
}

// WithRepositoryURLs sets an ordered list of upstream repository URLs.
// Fetches try them in order and fall through on 404. Replaces any previously
// configured URLs.
func WithRepositoryURLs(urls ...string) UpstreamOption {
	return func(u *Upstream) {
		u.baseURLs = u.baseURLs[:0]
		for _, raw := range urls {
			u.baseURLs = append(u.baseURLs, normalizeUpstreamURL(raw))
		}
	}
}

// normalizeUpstreamURL trims surrounding whitespace and a trailing slash so
// that "https://x/" and "https://x" compare equal.
func normalizeUpstreamURL(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), "/")
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(client *http.Client) UpstreamOption {
	return func(u *Upstream) {
		u.client = client
	}
}

// WithPerUpstreamTimeout caps a single per-upstream attempt. Zero disables.
func WithPerUpstreamTimeout(d time.Duration) UpstreamOption {
	return func(u *Upstream) {
		u.perTryTimeout = d
	}
}

// WithUserAgent sets the User-Agent header sent to upstream repositories.
// Identifying the client as a caching proxy helps avoid rate-limiting and
// blocklisting heuristics on upstreams such as Maven Central.
func WithUserAgent(ua string) UpstreamOption {
	return func(u *Upstream) {
		u.userAgent = ua
	}
}

// NewUpstream creates a new upstream repository client.
func NewUpstream(opts ...UpstreamOption) *Upstream {
	u := &Upstream{
		baseURLs: []string{DefaultRepositoryURL},
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
		userAgent:      "content-cache",
		artifactNegTTL: DefaultArtifactNegativeCacheTTL,
		metadataNegTTL: DefaultMetadataNegativeCacheTTL,
	}
	for _, opt := range opts {
		opt(u)
	}
	// Only enforce a per-try timeout when there's more than one upstream;
	// single-upstream callers keep their existing behaviour.
	if u.perTryTimeout == 0 && len(u.baseURLs) > 1 {
		u.perTryTimeout = DefaultPerUpstreamTimeout
	}
	return u
}

// tryEach iterates configured upstreams in order, calling fn for each.
// Returns the first non-ErrNotFound result. If all upstreams return
// ErrNotFound, returns ErrNotFound. The cacheKey is used for negative caching.
func (u *Upstream) tryEach(ctx context.Context, kind pathKind, cacheKey string, fn func(ctx context.Context, baseURL string) error) error {
	var lastErr error
	for _, baseURL := range u.baseURLs {
		if u.isNegativelyCached(ctx, baseURL, cacheKey) {
			lastErr = ErrNotFound
			continue
		}
		err := u.tryOne(ctx, baseURL, fn)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrNotFound) {
			u.markNegative(ctx, kind, baseURL, cacheKey)
			lastErr = ErrNotFound
			continue
		}
		// Non-404 error: return immediately rather than masking with a later 404.
		return err
	}
	if lastErr == nil {
		return ErrNotFound
	}
	return lastErr
}

// tryOne runs fn with a per-attempt timeout if configured.
func (u *Upstream) tryOne(ctx context.Context, baseURL string, fn func(ctx context.Context, baseURL string) error) error {
	if u.perTryTimeout <= 0 {
		return fn(ctx, baseURL)
	}
	tryCtx, cancel := context.WithTimeout(ctx, u.perTryTimeout)
	defer cancel()
	return fn(tryCtx, baseURL)
}

// primaryBaseURL returns the first configured base URL, used for URL formatting
// helpers where the chain is not meaningful.
func (u *Upstream) primaryBaseURL() string {
	if len(u.baseURLs) == 0 {
		return ""
	}
	return u.baseURLs[0]
}

// FetchMetadata fetches maven-metadata.xml for an artifact.
func (u *Upstream) FetchMetadata(ctx context.Context, groupID, artifactID string) (*MavenMetadata, error) {
	raw, err := u.FetchMetadataRaw(ctx, groupID, artifactID)
	if err != nil {
		return nil, err
	}
	var meta MavenMetadata
	if err := xml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("decoding metadata: %w", err)
	}
	return &meta, nil
}

// FetchMetadataRaw fetches raw maven-metadata.xml content.
func (u *Upstream) FetchMetadataRaw(ctx context.Context, groupID, artifactID string) ([]byte, error) {
	path := groupIDToPath(groupID) + "/" + artifactID + "/maven-metadata.xml"
	var result []byte
	err := u.tryEach(ctx, pathKindMetadata, "GET:"+path, func(ctx context.Context, baseURL string) error {
		data, err := u.getBody(ctx, baseURL+"/"+path)
		if err != nil {
			return err
		}
		result = data
		return nil
	})
	return result, err
}

// FetchArtifact fetches an artifact file (JAR, POM, etc.).
// Returns a ReadCloser that must be closed by the caller.
//
// Unlike short fetches, this does not apply a per-upstream timeout because
// the returned body must remain readable after the request returns. Total
// fetch duration is bounded by the underlying http.Client.Timeout.
func (u *Upstream) FetchArtifact(ctx context.Context, coord ArtifactCoordinate) (io.ReadCloser, int64, error) {
	path := coord.FullPath()
	cacheKey := "GET:" + path
	var lastErr error
	for _, baseURL := range u.baseURLs {
		if u.isNegativelyCached(ctx, baseURL, cacheKey) {
			lastErr = ErrNotFound
			continue
		}
		req, err := u.newRequest(ctx, http.MethodGet, baseURL+"/"+path)
		if err != nil {
			return nil, 0, err
		}
		resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
		if err != nil {
			return nil, 0, fmt.Errorf("performing request: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			u.markNegative(ctx, pathKindArtifact, baseURL, cacheKey)
			lastErr = ErrNotFound
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, 0, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(b))
		}
		return resp.Body, resp.ContentLength, nil
	}
	if lastErr == nil {
		return nil, 0, ErrNotFound
	}
	return nil, 0, lastErr
}

// FetchChecksum fetches a checksum file for an artifact.
func (u *Upstream) FetchChecksum(ctx context.Context, coord ArtifactCoordinate, checksumType string) (string, error) {
	path := coord.FullPath() + "." + checksumType
	var checksum string
	err := u.tryEach(ctx, pathKindArtifact, "GET:"+path, func(ctx context.Context, baseURL string) error {
		data, err := u.getBody(ctx, baseURL+"/"+path)
		if err != nil {
			return err
		}
		// Checksum files may contain just the hash or "hash  filename";
		// keep only the hash portion.
		s := strings.TrimSpace(string(data))
		if idx := strings.Index(s, " "); idx > 0 {
			s = s[:idx]
		}
		checksum = s
		return nil
	})
	return checksum, err
}

// HeadArtifact checks if an artifact exists and returns its size.
func (u *Upstream) HeadArtifact(ctx context.Context, coord ArtifactCoordinate) (int64, error) {
	path := coord.FullPath()
	var size int64
	err := u.tryEach(ctx, pathKindArtifact, "HEAD:"+path, func(ctx context.Context, baseURL string) error {
		req, err := u.newRequest(ctx, http.MethodHead, baseURL+"/"+path)
		if err != nil {
			return err
		}
		resp, err := u.client.Do(req)
		if err != nil {
			return fmt.Errorf("performing request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("upstream returned %d", resp.StatusCode)
		}
		size = resp.ContentLength
		return nil
	})
	return size, err
}

// FetchRootFile fetches a root-level file like archetype-catalog.xml.
func (u *Upstream) FetchRootFile(ctx context.Context, filename string) ([]byte, error) {
	var result []byte
	err := u.tryEach(ctx, pathKindMetadata, "GET:"+filename, func(ctx context.Context, baseURL string) error {
		data, err := u.getBody(ctx, baseURL+"/"+filename)
		if err != nil {
			return err
		}
		result = data
		return nil
	})
	return result, err
}

// newRequest constructs an outbound request with the configured User-Agent.
func (u *Upstream) newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if u.userAgent != "" {
		req.Header.Set("User-Agent", u.userAgent)
	}
	return req, nil
}

// getBody performs a GET and returns the body bytes, mapping 404 to ErrNotFound.
func (u *Upstream) getBody(ctx context.Context, url string) ([]byte, error) {
	req, err := u.newRequest(ctx, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// ArtifactURL returns the full URL for an artifact against the primary upstream.
// This is informational; actual fetches iterate the chain.
func (u *Upstream) ArtifactURL(coord ArtifactCoordinate) string {
	return u.primaryBaseURL() + "/" + coord.FullPath()
}

// MetadataURL returns the full URL for maven-metadata.xml against the primary
// upstream. This is informational; actual fetches iterate the chain.
func (u *Upstream) MetadataURL(groupID, artifactID string) string {
	return u.primaryBaseURL() + "/" + groupIDToPath(groupID) + "/" + artifactID + "/maven-metadata.xml"
}

// groupIDToPath converts a Maven groupId to a path (dots to slashes).
// e.g., "org.apache.commons" -> "org/apache/commons"
func groupIDToPath(groupID string) string {
	return strings.ReplaceAll(groupID, ".", "/")
}

// pathToGroupID converts a path back to a Maven groupId (slashes to dots).
// e.g., "org/apache/commons" -> "org.apache.commons"
func pathToGroupID(path string) string {
	return strings.ReplaceAll(path, "/", ".")
}
