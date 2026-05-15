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
)

// ErrNotFound is returned when an artifact is not found upstream.
var ErrNotFound = errors.New("artifact not found")

// Upstream fetches artifacts from an upstream Maven repository.
type Upstream struct {
	baseURL string
	client  *http.Client
}

// UpstreamOption configures an Upstream.
type UpstreamOption func(*Upstream)

// WithRepositoryURL sets the upstream repository URL.
func WithRepositoryURL(url string) UpstreamOption {
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

// NewUpstream creates a new upstream repository client.
func NewUpstream(opts ...UpstreamOption) *Upstream {
	u := &Upstream{
		baseURL: DefaultRepositoryURL,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// FetchMetadata fetches maven-metadata.xml for an artifact.
func (u *Upstream) FetchMetadata(ctx context.Context, groupID, artifactID string) (*MavenMetadata, error) {
	path := groupIDToPath(groupID) + "/" + artifactID + "/maven-metadata.xml"
	url := u.baseURL + "/" + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req)
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

	var meta MavenMetadata
	if err := xml.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding metadata: %w", err)
	}

	return &meta, nil
}

// FetchMetadataRaw fetches raw maven-metadata.xml content.
func (u *Upstream) FetchMetadataRaw(ctx context.Context, groupID, artifactID string) ([]byte, error) {
	path := groupIDToPath(groupID) + "/" + artifactID + "/maven-metadata.xml"
	url := u.baseURL + "/" + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
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

// FetchArtifact fetches an artifact file (JAR, POM, etc.).
// Returns a ReadCloser that must be closed by the caller.
func (u *Upstream) FetchArtifact(ctx context.Context, coord ArtifactCoordinate) (io.ReadCloser, int64, error) {
	url := u.ArtifactURL(coord)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, 0, fmt.Errorf("performing request: %w", err)
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

// FetchChecksum fetches a checksum file for an artifact.
func (u *Upstream) FetchChecksum(ctx context.Context, coord ArtifactCoordinate, checksumType string) (string, error) {
	url := u.ArtifactURL(coord) + "." + checksumType

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is constructed from operator-configured baseURL, not user input
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return "", fmt.Errorf("performing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading checksum: %w", err)
	}

	// Checksum files may contain just the hash or "hash  filename"
	// We only want the hash portion
	checksum := strings.TrimSpace(string(data))
	if idx := strings.Index(checksum, " "); idx > 0 {
		checksum = checksum[:idx]
	}

	return checksum, nil
}

// HeadArtifact checks if an artifact exists and returns its size.
func (u *Upstream) HeadArtifact(ctx context.Context, coord ArtifactCoordinate) (int64, error) {
	url := u.ArtifactURL(coord)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("performing request: %w", err)
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

// FetchRootFile fetches a root-level file like archetype-catalog.xml.
func (u *Upstream) FetchRootFile(ctx context.Context, filename string) ([]byte, error) {
	url := u.baseURL + "/" + filename

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := u.client.Do(req)
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

// ArtifactURL returns the full URL for an artifact.
func (u *Upstream) ArtifactURL(coord ArtifactCoordinate) string {
	return u.baseURL + "/" + coord.FullPath()
}

// MetadataURL returns the full URL for maven-metadata.xml.
func (u *Upstream) MetadataURL(groupID, artifactID string) string {
	return u.baseURL + "/" + groupIDToPath(groupID) + "/" + artifactID + "/maven-metadata.xml"
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
