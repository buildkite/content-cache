package rubygems

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// Upstream fetches content from the upstream RubyGems registry.
type Upstream struct {
	baseURL    string
	httpClient *http.Client
}

// UpstreamOption configures an Upstream.
type UpstreamOption func(*Upstream)

// WithRegistryURL sets the upstream registry URL.
func WithRegistryURL(url string) UpstreamOption {
	return func(u *Upstream) {
		u.baseURL = url
	}
}

// WithHTTPClient sets the HTTP client.
func WithHTTPClient(client *http.Client) UpstreamOption {
	return func(u *Upstream) {
		u.httpClient = client
	}
}

// NewUpstream creates a new Upstream client.
func NewUpstream(opts ...UpstreamOption) *Upstream {
	u := &Upstream{
		baseURL: DefaultUpstreamURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for large gem downloads
		},
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// CompactIndexResponse contains the response from a Compact Index endpoint.
type CompactIndexResponse struct {
	Body       io.ReadCloser
	StatusCode int // 200, 206 (partial), 304 (not modified), 416 (range not satisfiable)
	ETag       string
	ReprDigest string // SHA256 from Repr-Digest header
	Size       int64  // Content-Length
}

// FetchVersions fetches the /versions file from upstream.
// If etag is provided, sends If-None-Match header.
// If rangeStart > 0, sends Range header for incremental update.
func (u *Upstream) FetchVersions(ctx context.Context, etag string, rangeStart int64) (*CompactIndexResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/versions", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	u.setCompactIndexHeaders(req, etag, rangeStart)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching versions: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}

	return u.buildCompactIndexResponse(resp), nil
}

// FetchInfo fetches the /info/{gem} file from upstream.
func (u *Upstream) FetchInfo(ctx context.Context, gem string, etag string, rangeStart int64) (*CompactIndexResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/info/"+gem, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	u.setCompactIndexHeaders(req, etag, rangeStart)

	resp, err := u.httpClient.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, fmt.Errorf("fetching info: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}

	return u.buildCompactIndexResponse(resp), nil
}

// FetchNames fetches the /names file from upstream.
func (u *Upstream) FetchNames(ctx context.Context) (*CompactIndexResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/names", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	u.setCompactIndexHeaders(req, "", 0)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching names: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}

	return u.buildCompactIndexResponse(resp), nil
}

// SpecsResponse contains the response from fetching a specs file.
type SpecsResponse struct {
	Body       io.ReadCloser
	StatusCode int
	ETag       string
	Size       int64
}

// FetchSpecs fetches a specs file (specs.4.8.gz, latest_specs.4.8.gz, prerelease_specs.4.8.gz).
func (u *Upstream) FetchSpecs(ctx context.Context, specsType string, etag string) (*SpecsResponse, error) {
	url := fmt.Sprintf("%s/%s.4.8.gz", u.baseURL, specsType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching specs: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}

	return &SpecsResponse{
		Body:       resp.Body,
		StatusCode: resp.StatusCode,
		ETag:       resp.Header.Get("ETag"),
		Size:       resp.ContentLength,
	}, nil
}

// FetchGemspec fetches a gemspec file from quick/Marshal.4.8/.
func (u *Upstream) FetchGemspec(ctx context.Context, name, version, platform string) (io.ReadCloser, int64, error) {
	filename := gemspecFilename(name, version, platform) + ".gemspec.rz"
	url := fmt.Sprintf("%s/quick/Marshal.4.8/%s", u.baseURL, filename)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching gemspec: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return resp.Body, resp.ContentLength, nil
}

// FetchGem fetches a gem file from /gems/.
func (u *Upstream) FetchGem(ctx context.Context, filename string) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("%s/gems/%s", u.baseURL, filename)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.httpClient.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, 0, fmt.Errorf("fetching gem: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return resp.Body, resp.ContentLength, nil
}

// setCompactIndexHeaders sets headers for Compact Index requests.
// CRITICAL: Always use Accept-Encoding: identity to avoid gzip complications with Range.
func (u *Upstream) setCompactIndexHeaders(req *http.Request, etag string, rangeStart int64) {
	req.Header.Set("Accept-Encoding", "identity") // Critical for Range compatibility
	req.Header.Set("Accept", "text/plain")

	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if rangeStart > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
	}
}

// reprDigestRegex matches the SHA256 value in Repr-Digest header.
// Format: sha-256=:base64value:
var reprDigestRegex = regexp.MustCompile(`sha-256=:([A-Za-z0-9+/=]+):`)

// buildCompactIndexResponse builds a CompactIndexResponse from an HTTP response.
func (u *Upstream) buildCompactIndexResponse(resp *http.Response) *CompactIndexResponse {
	reprDigest := ""
	if rd := resp.Header.Get("Repr-Digest"); rd != "" {
		if matches := reprDigestRegex.FindStringSubmatch(rd); len(matches) > 1 {
			reprDigest = matches[1]
		}
	}

	return &CompactIndexResponse{
		Body:       resp.Body,
		StatusCode: resp.StatusCode,
		ETag:       resp.Header.Get("ETag"),
		ReprDigest: reprDigest,
		Size:       resp.ContentLength,
	}
}
