package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultRegistryURL is the default Docker Hub registry.
	DefaultRegistryURL = "https://registry-1.docker.io"

	// DefaultTimeout is the default timeout for upstream requests.
	DefaultTimeout = 30 * time.Second

	// DockerContentDigestHeader is the header containing the manifest digest.
	DockerContentDigestHeader = "Docker-Content-Digest"

	// maxManifestSize is the maximum size of a manifest response body (32 MB).
	maxManifestSize = 32 * 1024 * 1024
)

// Upstream fetches content from an upstream OCI registry.
type Upstream struct {
	baseURL   string
	client    *http.Client
	authCache *AuthCache
	username  string
	password  string
}

// UpstreamOption configures an Upstream.
type UpstreamOption func(*Upstream)

// WithRegistryURL sets the upstream registry URL.
func WithRegistryURL(url string) UpstreamOption {
	return func(u *Upstream) {
		u.baseURL = strings.TrimSuffix(url, "/")
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(client *http.Client) UpstreamOption {
	return func(u *Upstream) {
		u.client = client
	}
}

// WithBasicAuth sets credentials for registry authentication.
func WithBasicAuth(username, password string) UpstreamOption {
	return func(u *Upstream) {
		u.username = username
		u.password = password
	}
}

// NewUpstream creates a new upstream registry client.
func NewUpstream(opts ...UpstreamOption) *Upstream {
	u := &Upstream{
		baseURL: DefaultRegistryURL,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
		authCache: NewAuthCache(),
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// CheckVersion verifies the registry supports the v2 API.
func (u *Upstream) CheckVersion(ctx context.Context) error {
	url := fmt.Sprintf("%s/v2/", u.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.doWithAuth(ctx, req, "")
	if err != nil {
		// ErrUnauthorized is acceptable - it means the registry exists but requires auth
		if errors.Is(err, ErrUnauthorized) {
			return nil
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

// FetchManifest retrieves a manifest by tag or digest.
// Returns the manifest content, media type, and digest.
func (u *Upstream) FetchManifest(ctx context.Context, name, reference string) ([]byte, string, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", u.baseURL, name, reference)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("creating request: %w", err)
	}

	// Accept multiple manifest types
	req.Header.Set("Accept", strings.Join(DefaultAcceptHeader, ", "))

	scope := BuildScope(name, "pull")
	resp, err := u.doWithAuth(ctx, req, scope)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", "", ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		return nil, "", "", fmt.Errorf("reading response: %w", err)
	}

	mediaType := resp.Header.Get("Content-Type")
	digest := resp.Header.Get(DockerContentDigestHeader)

	return content, mediaType, digest, nil
}

// HeadManifest checks manifest existence and returns the digest, size, and media type.
func (u *Upstream) HeadManifest(ctx context.Context, name, reference string) (string, int64, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", u.baseURL, name, reference)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", 0, "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", strings.Join(DefaultAcceptHeader, ", "))

	scope := BuildScope(name, "pull")
	resp, err := u.doWithAuth(ctx, req, scope)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", 0, "", ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return "", 0, "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	digest := resp.Header.Get(DockerContentDigestHeader)
	mediaType := resp.Header.Get("Content-Type")
	return digest, resp.ContentLength, mediaType, nil
}

// FetchBlob retrieves a blob by digest.
// Returns a ReadCloser that must be closed by the caller.
func (u *Upstream) FetchBlob(ctx context.Context, name, digest string) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", u.baseURL, name, digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	scope := BuildScope(name, "pull")
	resp, err := u.doWithAuth(ctx, req, scope)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, resp.ContentLength, nil
}

// HeadBlob checks blob existence and returns the size.
func (u *Upstream) HeadBlob(ctx context.Context, name, digest string) (int64, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", u.baseURL, name, digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}

	scope := BuildScope(name, "pull")
	resp, err := u.doWithAuth(ctx, req, scope)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	return resp.ContentLength, nil
}

// doWithAuth performs an HTTP request with authentication retry.
func (u *Upstream) doWithAuth(ctx context.Context, req *http.Request, scope string) (*http.Response, error) {
	// Try with cached token first
	if scope != "" {
		if token := u.authCache.GetToken(scope); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}

	// If not 401, return the response
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// Close the 401 response body
	_ = resp.Body.Close()

	// Parse the WWW-Authenticate header
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if wwwAuth == "" {
		return nil, ErrUnauthorized
	}

	challenge, err := ParseWWWAuthenticate(wwwAuth)
	if err != nil {
		return nil, fmt.Errorf("parsing auth challenge: %w", err)
	}

	// Override scope from challenge if provided
	if challenge.Scope != "" {
		scope = challenge.Scope
	}

	// Fetch a new token
	tokenResp, err := FetchToken(ctx, u.client, challenge, u.username, u.password)
	if err != nil {
		return nil, fmt.Errorf("fetching token: %w", err)
	}

	// Cache the token
	u.authCache.SetToken(scope, tokenResp.Token, tokenResp.ExpiresIn)

	// Clone the request for retry (original request body may be consumed)
	retryReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), nil) //nolint:gosec // retryReq reuses the original request URL which targets a configured upstream registry
	if err != nil {
		return nil, fmt.Errorf("creating retry request: %w", err)
	}

	// Copy headers from original request
	for key, values := range req.Header {
		for _, value := range values {
			retryReq.Header.Add(key, value)
		}
	}

	// Set the new token
	retryReq.Header.Set("Authorization", "Bearer "+tokenResp.Token)

	// Retry the request
	return u.client.Do(retryReq) //nolint:gosec // request targets operator-configured OCI registry, not user-controlled
}
