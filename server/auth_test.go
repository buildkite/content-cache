package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"github.com/wolfeidau/content-cache/auth"
	"github.com/wolfeidau/content-cache/telemetry"
)

func TestAuthMiddleware_NoToken_NoOp(t *testing.T) {
	s := &Server{config: Config{AuthToken: ""}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	s := &Server{config: Config{AuthToken: "test-token-123"}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	s := &Server{config: Config{AuthToken: "test-token-123"}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer, Basic realm="content-cache"`, rec.Header().Get("WWW-Authenticate"))

	var body map[string]string
	err := json.NewDecoder(rec.Body).Decode(&body)
	require.NoError(t, err)
	require.Equal(t, "unauthorized", body["error"])
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	s := &Server{config: Config{AuthToken: "test-token-123"}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthMiddleware_BasicAuth_ValidToken(t *testing.T) {
	s := &Server{config: Config{AuthToken: "test-token-123"}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	req.SetBasicAuth("user", "test-token-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_BasicAuth_WrongPassword(t *testing.T) {
	s := &Server{config: Config{AuthToken: "test-token-123"}}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/npm/react", nil)
	req.SetBasicAuth("user", "wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthMiddleware_ExemptPaths(t *testing.T) {
	s := &Server{
		config: Config{AuthToken: "test-token-123"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/metrics"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "path %s should be exempt from auth", path)
		})
	}
}

func TestProtocolFromPath(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/health", ""},
		{"/metrics", ""},
		{"/stats", "admin"},
		{"/admin/gc", "admin"},
		{"/admin/gc/status", "admin"},
		{"/admin/anything/new", "admin"},
		{"/npm/react", "npm"},
		{"/npm/@scope/pkg", "npm"},
		{"/v2/", "oci"},
		{"/v2/library/alpine/manifests/latest", "oci"},
		{"/pypi/simple/requests/", "pypi"},
		{"/maven/org/example/artifact/1.0/artifact-1.0.jar", "maven"},
		{"/rubygems/gems/rails-7.0.0.gem", "rubygems"},
		{"/git/github.com/org/repo.git/info/refs", "git"},
		{"/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0", "sumdb"},
		{"/goproxy/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0", "sumdb"},
		{"/goproxy/github.com/foo/bar/@v/list", "goproxy"},
		{"/buildcache/", "buildcache"},
		{"/buildcache/objects/abc123", "buildcache"},
		{"/github.com/foo/bar/@v/list", "unknown"},
		{"/", "unknown"},
		{"/unknown/path", "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			require.Equal(t, tc.expected, protocolFromPath(tc.path))
		})
	}
}

func TestAuthMiddleware_ProtectedPaths(t *testing.T) {
	s := &Server{
		config: Config{AuthToken: "test-token-123"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/stats", "/admin/gc", "/admin/gc/status", "/npm/react", "/v2/", "/git/github.com/org/repo.git/info/refs"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "path %s should require auth", path)
		})
	}
}

// testOIDCProvider is a self-contained fake OIDC provider for use in tests.
// It serves a discovery document and JWKS endpoint, and can mint signed JWTs.
type testOIDCProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	p := &testOIDCProvider{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   p.server.URL,
			"jwks_uri": p.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := &key.PublicKey
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": "test-key-1",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})

	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// token mints a signed JWT accepted by this provider with the given audience.
func (p *testOIDCProvider) token(t *testing.T, audience []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": p.server.URL,
		"sub": "test-subject",
		"aud": audience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test-key-1"
	signed, err := tok.SignedString(p.key)
	require.NoError(t, err)
	return signed
}

// validator creates an OIDCValidator backed by this provider granting the given permissions.
func (p *testOIDCProvider) validator(t *testing.T, permissions []string) *auth.OIDCValidator {
	t.Helper()
	policies := []auth.TrustPolicy{{
		Name:           "test-policy",
		Issuer:         p.server.URL,
		Audience:       []string{"test-audience"},
		RequiredClaims: map[string]any{},
		Permissions:    permissions,
	}}
	v, err := auth.NewOIDCValidator(context.Background(), policies, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return v
}

func TestOIDCMiddleware_ExemptPaths(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/metrics"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestOIDCMiddleware_MissingAuthHeader(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExtractToken(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{"bearer lowercase", "bearer mytoken", "mytoken", true},
		{"bearer uppercase", "BEARER mytoken", "mytoken", true},
		{"bearer canonical", "Bearer mytoken", "mytoken", true},
		{"bearer empty token", "Bearer ", "", false},
		{"basic valid", "", "", false}, // set via SetBasicAuth below
		{"basic malformed base64", "Basic !!!notbase64!!!", "", false},
		{"digest scheme", "Digest abc123", "", false},
		{"no header", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.name == "basic valid" {
				req.SetBasicAuth("user", "mytoken")
				token, ok := extractToken(req)
				require.True(t, ok)
				require.Equal(t, "mytoken", token)
				return
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			token, ok := extractToken(req)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantToken, token)
		})
	}
}

func TestOIDCMiddleware_BasicAuth_InvalidToken(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Basic auth with a non-JWT password is rejected during OIDC validation.
	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.SetBasicAuth("user", "not-a-valid-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestOIDCMiddleware_UnknownScheme(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// An unsupported scheme (e.g. Digest) is not extracted as a token → 401.
	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.Header.Set("Authorization", "Digest abc123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestOIDCMiddleware_BasicAuth_ValidToken(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Basic auth where password = valid OIDC token (e.g. pip using __token__:$CACHE_TOKEN).
	token := oidcProvider.token(t, []string{"test-audience"})
	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.SetBasicAuth("__token__", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOIDCMiddleware_InvalidToken(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestOIDCMiddleware_InsufficientPermission(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	// validator only grants "npm", not "goproxy"
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"npm"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := oidcProvider.token(t, []string{"test-audience"})
	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, `Bearer error="insufficient_scope"`, rec.Header().Get("WWW-Authenticate"))
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, "forbidden", body["error"])
}

func TestOIDCMiddleware_ValidToken_ClaimsStoredInContext(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	var gotClaims *auth.ValidatedClaims
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.GetClaims(r.Context())
		if ok {
			gotClaims = claims
		}
		w.WriteHeader(http.StatusOK)
	}))

	token := oidcProvider.token(t, []string{"test-audience"})
	req := httptest.NewRequest(http.MethodGet, "/goproxy/github.com/foo/bar/@v/list", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, gotClaims, "claims should be stored in request context")
	require.Equal(t, oidcProvider.server.URL, gotClaims.Issuer)
}

func TestOIDCMiddleware_SetsAuthOutcomeTags(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)

	cases := []struct {
		name        string
		permissions []string
		path        string
		setupReq    func(*http.Request, *testOIDCProvider) *http.Request
		wantStatus  int
		wantOutcome telemetry.AuthOutcome
	}{
		{
			name:        "allowed",
			permissions: []string{"goproxy"},
			path:        "/goproxy/github.com/foo/bar/@v/list",
			setupReq: func(r *http.Request, p *testOIDCProvider) *http.Request {
				r.Header.Set("Authorization", "Bearer "+p.token(t, []string{"test-audience"}))
				return r
			},
			wantStatus:  http.StatusOK,
			wantOutcome: telemetry.AuthOutcomeAllowed,
		},
		{
			name:        "unauthorized_missing_header",
			permissions: []string{"goproxy"},
			path:        "/goproxy/github.com/foo/bar/@v/list",
			setupReq:    func(r *http.Request, _ *testOIDCProvider) *http.Request { return r },
			wantStatus:  http.StatusUnauthorized,
			wantOutcome: telemetry.AuthOutcomeUnauthorized,
		},
		{
			name:        "unauthorized_invalid_token",
			permissions: []string{"goproxy"},
			path:        "/goproxy/github.com/foo/bar/@v/list",
			setupReq: func(r *http.Request, _ *testOIDCProvider) *http.Request {
				r.Header.Set("Authorization", "Bearer not-a-valid-jwt")
				return r
			},
			wantStatus:  http.StatusUnauthorized,
			wantOutcome: telemetry.AuthOutcomeUnauthorized,
		},
		{
			name:        "forbidden_insufficient_permission",
			permissions: []string{"npm"},
			path:        "/goproxy/github.com/foo/bar/@v/list",
			setupReq: func(r *http.Request, p *testOIDCProvider) *http.Request {
				r.Header.Set("Authorization", "Bearer "+p.token(t, []string{"test-audience"}))
				return r
			},
			wantStatus:  http.StatusForbidden,
			wantOutcome: telemetry.AuthOutcomeForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				config: Config{OIDCValidator: oidcProvider.validator(t, tc.permissions)},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := telemetry.InjectTags(httptest.NewRequest(http.MethodGet, tc.path, nil))
			req = tc.setupReq(req, oidcProvider)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code)
			require.Equal(t, tc.wantOutcome, telemetry.GetTags(req).AuthOutcome)
		})
	}
}

func TestOIDCMiddleware_UnknownPath(t *testing.T) {
	oidcProvider := newTestOIDCProvider(t)
	s := &Server{
		config: Config{OIDCValidator: oidcProvider.validator(t, []string{"goproxy", "npm"})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	handler := s.oidcMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// A valid token for an unrecognised path gets "unknown" protocol,
	// which no specific permission grants → 403.
	token := oidcProvider.token(t, []string{"test-audience"})
	req := httptest.NewRequest(http.MethodGet, "/unknown/path", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}
