package pypi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	// DefaultSimpleURL is the default PyPI Simple API URL.
	DefaultSimpleURL = "https://pypi.org/simple/"

	// DefaultTimeout is the default timeout for upstream requests.
	DefaultTimeout = 30 * time.Second
)

// ErrNotFound is returned when a project is not found.
var ErrNotFound = errors.New("not found")

// Upstream fetches packages from an upstream PyPI Simple API.
type Upstream struct {
	baseURL  string
	username string
	password string
	client   *http.Client
}

// UpstreamOption configures an Upstream.
type UpstreamOption func(*Upstream)

// WithSimpleURL sets the upstream Simple API URL. Any userinfo (user:pass@) is
// stripped from the URL and applied as Basic auth on every upstream request —
// including file downloads, which the index commonly serves from a different
// host (e.g. a CDN) that requires the same credentials.
func WithSimpleURL(rawURL string) UpstreamOption {
	return func(u *Upstream) {
		trimmed := strings.TrimSuffix(rawURL, "/") + "/"
		if parsed, err := url.Parse(trimmed); err == nil && parsed.User != nil {
			u.username = parsed.User.Username()
			u.password, _ = parsed.User.Password()
			parsed.User = nil
			trimmed = parsed.String()
		}
		u.baseURL = trimmed
	}
}

// setAuth applies the upstream Basic credentials to a request, if configured.
func (u *Upstream) setAuth(req *http.Request) {
	if u.username != "" || u.password != "" {
		req.SetBasicAuth(u.username, u.password)
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(client *http.Client) UpstreamOption {
	return func(u *Upstream) {
		u.client = client
	}
}

// NewUpstream creates a new upstream PyPI client.
func NewUpstream(opts ...UpstreamOption) *Upstream {
	u := &Upstream{
		baseURL: DefaultSimpleURL,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// FetchProjectPage fetches the project page for a package.
// Returns the raw response body and content type.
func (u *Upstream) FetchProjectPage(ctx context.Context, project string) ([]byte, string, error) {
	normalized := NormalizeProjectName(project)
	reqURL := u.baseURL + normalized + "/"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil) //nolint:gosec // reqURL is constructed from operator-configured baseURL, not user input
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}

	// Request JSON preferred, fallback to HTML
	req.Header.Set("Accept", ContentTypeJSON+", "+ContentTypeHTML+";q=0.9")
	u.setAuth(req)

	resp, err := u.client.Do(req) //nolint:gosec // request targets operator-configured upstream, not user-controlled
	if err != nil {
		return nil, "", fmt.Errorf("performing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	return body, contentType, nil
}

// FetchFile fetches a file (wheel or sdist) from the given URL.
// Returns a ReadCloser that must be closed by the caller.
func (u *Upstream) FetchFile(ctx context.Context, fileURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	u.setAuth(req)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// ParsedFile represents a file parsed from the project page HTML.
type ParsedFile struct {
	Filename       string
	URL            string
	Hashes         map[string]string
	RequiresPython string
	GPGSig         bool
	Yanked         any // bool or string
}

// ParseProjectPageHTML parses an HTML project page and extracts file information.
func ParseProjectPageHTML(body []byte, baseURL string) ([]ParsedFile, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}

	var files []ParsedFile
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			file := parseAnchor(n, baseURL)
			if file.Filename != "" {
				files = append(files, file)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return files, nil
}

// parseAnchor extracts file information from an anchor element.
func parseAnchor(n *html.Node, baseURL string) ParsedFile {
	var file ParsedFile
	var href string

	for _, attr := range n.Attr {
		switch attr.Key {
		case "href":
			href = attr.Val
		case "data-requires-python":
			file.RequiresPython = html.UnescapeString(attr.Val)
		case "data-gpg-sig":
			file.GPGSig = attr.Val == "true"
		case "data-yanked":
			if attr.Val == "" {
				file.Yanked = true
			} else {
				file.Yanked = attr.Val
			}
		}
	}

	if href == "" {
		return file
	}

	// Parse URL and extract hash from fragment
	parsedURL, err := url.Parse(href)
	if err != nil {
		return file
	}

	// Resolve relative URLs
	if !parsedURL.IsAbs() {
		base, err := url.Parse(baseURL)
		if err == nil {
			parsedURL = base.ResolveReference(parsedURL)
		}
	}

	// Extract hash from fragment (e.g., #sha256=abc123)
	if parsedURL.Fragment != "" {
		file.Hashes = parseHashFragment(parsedURL.Fragment)
	}

	// Remove fragment for the URL
	parsedURL.Fragment = ""
	file.URL = parsedURL.String()

	// Extract filename from URL path
	file.Filename = extractFilename(parsedURL.Path)

	return file
}

// hashFragmentRegex matches hash fragments like "sha256=abc123" or "md5=def456".
var hashFragmentRegex = regexp.MustCompile(`^([a-z0-9]+)=([a-f0-9]+)$`)

// parseHashFragment parses a URL fragment like "sha256=abc123" into a hash map.
func parseHashFragment(fragment string) map[string]string {
	matches := hashFragmentRegex.FindStringSubmatch(fragment)
	if len(matches) != 3 {
		return nil
	}
	return map[string]string{
		matches[1]: matches[2],
	}
}

// extractFilename extracts the filename from a URL path.
func extractFilename(path string) string {
	// Find the last path segment
	idx := strings.LastIndex(path, "/")
	if idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// normalizeRegex matches runs of separators to normalize.
var normalizeRegex = regexp.MustCompile(`[-_.]+`)

// NormalizeProjectName normalizes a project name according to PEP 503.
// Lowercases and replaces runs of '.', '-', '_' with a single '-'.
// Returns an empty string if the input is empty or contains only separators.
func NormalizeProjectName(name string) string {
	if name == "" {
		return ""
	}
	normalized := strings.ToLower(normalizeRegex.ReplaceAllString(name, "-"))
	// Trim leading/trailing hyphens that might result from normalization
	normalized = strings.Trim(normalized, "-")
	return normalized
}
