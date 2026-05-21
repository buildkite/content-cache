package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestGitHubAppAuthBasicAuthRequestsRepoScopedToken(t *testing.T) {
	privateKey, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	var requests []gitHubAppTokenRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/app/installations/67890/access_tokens" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/app/installations/67890/access_tokens")
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want %q", got, "application/vnd.github+json")
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != gitHubAPIVersion {
			t.Errorf("X-GitHub-Api-Version = %q, want %q", got, gitHubAPIVersion)
		}

		appJWT := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if appJWT == "" {
			t.Error("Authorization bearer token is empty")
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		claims := jwt.MapClaims{}
		parsed, err := jwt.NewParser(jwt.WithoutClaimsValidation()).ParseWithClaims(appJWT, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodRS256 {
				t.Errorf("jwt method = %v, want %v", token.Method, jwt.SigningMethodRS256)
			}
			return &privateKey.PublicKey, nil
		})
		if err != nil {
			t.Errorf("parse app jwt: %v", err)
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		if !parsed.Valid {
			t.Error("parsed app jwt is invalid")
		}
		if got := claims["iss"]; got != "12345" {
			t.Errorf("iss = %v, want %q", got, "12345")
		}
		if got := claims["iat"]; got != float64(now.Add(-60*time.Second).Unix()) {
			t.Errorf("iat = %v, want %d", got, now.Add(-60*time.Second).Unix())
		}
		if got := claims["exp"]; got != float64(now.Add(10*time.Minute).Unix()) {
			t.Errorf("exp = %v, want %d", got, now.Add(10*time.Minute).Unix())
		}

		var body gitHubAppTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode token request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requests = append(requests, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(gitHubAppTokenResponse{
			Token:     fmt.Sprintf("token-%d", len(requests)),
			ExpiresAt: now.Add(time.Hour),
		})
	}))
	t.Cleanup(api.Close)

	auth, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	}, withGitHubAppAPIURL(api.URL), withGitHubAppClock(func() time.Time { return now }))
	require.NoError(t, err)

	username, password, err := auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
	require.NoError(t, err)
	require.Equal(t, "x-access-token", username)
	require.Equal(t, "token-1", password)
	require.Len(t, requests, 1)
	require.Equal(t, []string{"content-cache"}, requests[0].Repositories)
	require.Equal(t, map[string]string{"contents": "read"}, requests[0].Permissions)

	username, password, err = auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "Buildkite/content-cache"})
	require.NoError(t, err)
	require.Equal(t, "x-access-token", username)
	require.Equal(t, "token-1", password)
	require.Len(t, requests, 1, "same canonical owner/repo should reuse the cached token")

	_, password, err = auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/other"})
	require.NoError(t, err)
	require.Equal(t, "token-2", password)
	require.Len(t, requests, 2)
	require.Equal(t, []string{"other"}, requests[1].Repositories)
}

func TestGitHubAppAuthMintsDifferentRepoTokensConcurrently(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	requestStarted := make(chan string, 2)
	releaseRequests := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRequests)
		})
	}
	defer release()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body gitHubAppTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode token request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.Repositories) != 1 {
			t.Errorf("repositories = %v, want one repository", body.Repositories)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		repoName := body.Repositories[0]
		requestStarted <- repoName
		<-releaseRequests

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(gitHubAppTokenResponse{
			Token:     "token-" + repoName,
			ExpiresAt: now.Add(time.Hour),
		})
	}))
	t.Cleanup(api.Close)

	auth, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	}, withGitHubAppAPIURL(api.URL), withGitHubAppClock(func() time.Time { return now }))
	require.NoError(t, err)

	type authResult struct {
		password string
		err      error
	}
	results := make(chan authResult, 2)

	go func() {
		_, password, err := auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/one"})
		results <- authResult{password: password, err: err}
	}()

	require.Equal(t, "one", receiveRepoName(t, requestStarted, time.Second))

	go func() {
		_, password, err := auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/two"})
		results <- authResult{password: password, err: err}
	}()

	require.Equal(t, "two", receiveRepoName(t, requestStarted, 200*time.Millisecond))
	release()

	var passwords []string
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		passwords = append(passwords, result.password)
	}
	require.ElementsMatch(t, []string{"token-one", "token-two"}, passwords)
}

func TestGitHubAppAuthSharedMintSurvivesLeaderCancellation(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var requestStartedOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRequest)
		})
	}
	defer release()

	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		requestStartedOnce.Do(func() {
			close(requestStarted)
		})
		<-releaseRequest

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(gitHubAppTokenResponse{
			Token:     "token-shared",
			ExpiresAt: now.Add(time.Hour),
		})
	}))
	t.Cleanup(api.Close)

	auth, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	}, withGitHubAppAPIURL(api.URL), withGitHubAppClock(func() time.Time { return now }))
	require.NoError(t, err)

	type authResult struct {
		password string
		err      error
	}
	leaderResults := make(chan authResult, 1)
	waiterResults := make(chan authResult, 1)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	go func() {
		_, password, err := auth.BasicAuth(leaderCtx, RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
		leaderResults <- authResult{password: password, err: err}
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for GitHub App token request")
	}

	go func() {
		_, password, err := auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
		waiterResults <- authResult{password: password, err: err}
	}()

	cancelLeader()
	leaderResult := <-leaderResults
	require.ErrorIs(t, leaderResult.err, context.Canceled)

	release()
	waiterResult := <-waiterResults
	require.NoError(t, waiterResult.err)
	require.Equal(t, "token-shared", waiterResult.password)
	require.Equal(t, int32(1), requests.Load(), "waiter should share the in-flight token mint, not start a second request")
}

func TestGitHubAppAuthFallsBackToValidCachedTokenOnRefreshFailure(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(gitHubAppTokenResponse{
				Token:     "token-cached",
				ExpiresAt: now.Add(2 * time.Minute),
			})
		case 2:
			http.Error(w, "temporary github outage", http.StatusServiceUnavailable)
		case 3:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(gitHubAppTokenResponse{
				Token:     "token-refreshed",
				ExpiresAt: now.Add(time.Hour),
			})
		default:
			t.Errorf("unexpected token request %d", requests.Load())
			http.Error(w, "unexpected token request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(api.Close)

	auth, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	}, withGitHubAppAPIURL(api.URL), withGitHubAppClock(func() time.Time { return now }))
	require.NoError(t, err)

	_, password, err := auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
	require.NoError(t, err)
	require.Equal(t, "token-cached", password)

	_, password, err = auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
	require.NoError(t, err)
	require.Equal(t, "token-cached", password)

	_, password, err = auth.BasicAuth(context.Background(), RepoRef{Host: "github.com", RepoPath: "buildkite/content-cache"})
	require.NoError(t, err)
	require.Equal(t, "token-refreshed", password)
	require.Equal(t, int32(3), requests.Load())
}

func TestGitHubAppAuthRejectsNonGitHubRepo(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)

	auth, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	})
	require.NoError(t, err)

	_, _, err = auth.BasicAuth(context.Background(), RepoRef{Host: "gitlab.com", RepoPath: "buildkite/content-cache"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "only supports github.com")
}

func TestNewGitHubAppAuthValidation(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)

	_, err := NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     "installation",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "token_scope")

	_, err = NewGitHubAppAuth(GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     "not pem",
		TokenScope:     GitHubAppTokenScopeRequestedRepo,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "private_key")
}

func testGitHubAppPrivateKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return key, string(pem.EncodeToMemory(block))
}

func receiveRepoName(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()

	select {
	case repo := <-ch:
		return repo
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for GitHub App token request")
		return ""
	}
}
