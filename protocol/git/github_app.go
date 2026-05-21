package git

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

const (
	// GitHubAppTokenScopeRequestedRepo scopes each installation token to the
	// repository named by the upstream Git request.
	GitHubAppTokenScopeRequestedRepo = "requested_repo"

	gitHubAPIURL              = "https://api.github.com"
	gitHubAPIVersion          = "2026-03-10"
	gitHubAppUsername         = "x-access-token"
	gitHubAppTokenRefreshSkew = 5 * time.Minute
)

// GitHubAppAuthConfig configures GitHub App installation authentication.
type GitHubAppAuthConfig struct {
	AppID          string
	InstallationID string
	PrivateKey     string
	TokenScope     string
}

// GitHubAppAuth resolves repo-scoped installation tokens for GitHub HTTPS Git access.
type GitHubAppAuth struct {
	appID          string
	installationID string
	privateKey     *rsa.PrivateKey
	tokenScope     string
	client         *http.Client
	apiURL         string
	now            func() time.Time

	mu         sync.Mutex
	tokens     map[string]gitHubAppCachedToken
	tokenGroup singleflight.Group
}

type gitHubAppCachedToken struct {
	token     string
	expiresAt time.Time
}

// GitHubAppAuthOption configures GitHub App authentication.
type GitHubAppAuthOption func(*GitHubAppAuth)

func withGitHubAppAPIURL(apiURL string) GitHubAppAuthOption {
	return func(a *GitHubAppAuth) {
		a.apiURL = strings.TrimRight(apiURL, "/")
	}
}

func withGitHubAppClock(now func() time.Time) GitHubAppAuthOption {
	return func(a *GitHubAppAuth) {
		a.now = now
	}
}

// NewGitHubAppAuth creates a GitHub App auth provider for upstream Git requests.
func NewGitHubAppAuth(cfg GitHubAppAuthConfig, opts ...GitHubAppAuthOption) (*GitHubAppAuth, error) {
	if cfg.AppID == "" {
		return nil, fmt.Errorf("github_app app_id is required")
	}
	if cfg.InstallationID == "" {
		return nil, fmt.Errorf("github_app installation_id is required")
	}
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("github_app private_key is required")
	}
	if cfg.TokenScope != GitHubAppTokenScopeRequestedRepo {
		return nil, fmt.Errorf("github_app token_scope must be %q", GitHubAppTokenScopeRequestedRepo)
	}

	privateKey, err := parseGitHubAppPrivateKey([]byte(cfg.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parsing github_app private_key: %w", err)
	}

	a := &GitHubAppAuth{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		privateKey:     privateKey,
		tokenScope:     cfg.TokenScope,
		client:         &http.Client{Timeout: 30 * time.Second},
		apiURL:         gitHubAPIURL,
		now:            time.Now,
		tokens:         make(map[string]gitHubAppCachedToken),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.client == nil {
		return nil, fmt.Errorf("github_app http client is nil")
	}
	if a.apiURL == "" {
		return nil, fmt.Errorf("github_app api url is empty")
	}
	if a.now == nil {
		return nil, fmt.Errorf("github_app clock is nil")
	}

	return a, nil
}

// BasicAuth returns credentials suitable for GitHub HTTPS Git access.
func (a *GitHubAppAuth) BasicAuth(ctx context.Context, repo RepoRef) (string, string, error) {
	token, err := a.installationToken(ctx, repo)
	if err != nil {
		return "", "", err
	}
	return gitHubAppUsername, token, nil
}

func (a *GitHubAppAuth) installationToken(ctx context.Context, repo RepoRef) (string, error) {
	_, repoName, cacheKey, err := canonicalGitHubRepo(repo)
	if err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	now := a.now()
	a.mu.Lock()
	if cached, ok := a.tokens[cacheKey]; ok && now.Before(cached.expiresAt.Add(-gitHubAppTokenRefreshSkew)) {
		a.mu.Unlock()
		return cached.token, nil
	}
	a.mu.Unlock()

	ch := a.tokenGroup.DoChan(cacheKey, func() (any, error) {
		now := a.now()
		a.mu.Lock()
		cached, hasCached := a.tokens[cacheKey]
		if hasCached && now.Before(cached.expiresAt.Add(-gitHubAppTokenRefreshSkew)) {
			a.mu.Unlock()
			return cached.token, nil
		}
		a.mu.Unlock()

		token, expiresAt, err := a.requestInstallationToken(context.WithoutCancel(ctx), repoName, now)
		if err != nil {
			if hasCached && a.now().Before(cached.expiresAt) {
				return cached.token, nil
			}
			return "", err
		}

		a.mu.Lock()
		a.tokens[cacheKey] = gitHubAppCachedToken{token: token, expiresAt: expiresAt}
		a.mu.Unlock()
		return token, nil
	})

	select {
	case result := <-ch:
		if result.Err != nil {
			return "", result.Err
		}
		token, ok := result.Val.(string)
		if !ok {
			return "", fmt.Errorf("github_app token cache returned %T, not string", result.Val)
		}
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func canonicalGitHubRepo(repo RepoRef) (owner, name, key string, err error) {
	if !strings.EqualFold(repo.Host, "github.com") {
		return "", "", "", fmt.Errorf("github_app only supports github.com, got %q", repo.Host)
	}

	owner, name, ok := strings.Cut(repo.RepoPath, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", "", fmt.Errorf("github_app requires GitHub repo path owner/repo, got %q", repo.RepoPath)
	}

	return owner, name, strings.ToLower(owner + "/" + name), nil
}

func (a *GitHubAppAuth) requestInstallationToken(ctx context.Context, repoName string, now time.Time) (string, time.Time, error) {
	appJWT, err := a.signedJWT(now)
	if err != nil {
		return "", time.Time{}, err
	}

	body := gitHubAppTokenRequest{
		Repositories: []string{repoName},
		Permissions:  map[string]string{"contents": "read"},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return "", time.Time{}, fmt.Errorf("encoding github_app token request: %w", err)
	}

	endpoint := a.apiURL + "/app/installations/" + url.PathEscape(a.installationID) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("creating github_app token request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("requesting github_app installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", time.Time{}, fmt.Errorf("github_app installation token returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded gitHubAppTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding github_app installation token: %w", err)
	}
	if decoded.Token == "" {
		return "", time.Time{}, fmt.Errorf("github_app installation token response omitted token")
	}
	if decoded.ExpiresAt.IsZero() {
		return "", time.Time{}, fmt.Errorf("github_app installation token response omitted expires_at")
	}

	return decoded.Token, decoded.ExpiresAt, nil
}

func (a *GitHubAppAuth) signedJWT(now time.Time) (string, error) {
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": a.appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(a.privateKey)
	if err != nil {
		return "", fmt.Errorf("signing github_app jwt: %w", err)
	}
	return signed, nil
}

type gitHubAppTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

type gitHubAppTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func parseGitHubAppPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(data))
	if block == nil {
		return nil, fmt.Errorf("missing PEM block")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, not *rsa.PrivateKey", parsed)
	}
	return key, nil
}
