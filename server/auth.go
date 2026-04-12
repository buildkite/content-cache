package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/buildkite/content-cache/auth"
	"github.com/buildkite/content-cache/telemetry"
)

// authMiddleware returns middleware that validates inbound authentication.
// Accepts both "Authorization: Bearer <token>" and "Authorization: Basic base64(user:token)"
// headers — the password field is treated as the token, enabling tools like pip, Maven,
// and Bundler that cannot send Bearer headers.
// When AuthToken is empty, the middleware is a no-op (allows unauthenticated access).
// Exact paths /health and /metrics are exempt from authentication.
//
// Note: this middleware does not set AuthOutcome on request tags because it runs
// before protocol-specific middleware and does not have per-protocol context.
// Auth observability is only available when using oidcMiddleware.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.config.AuthToken == "" {
		return next
	}

	tokenBytes := []byte(s.config.AuthToken)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt exact paths for health checks and metrics.
		if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		token, ok := extractToken(r)
		if !ok {
			unauthorizedResponse(w)
			return
		}

		if subtle.ConstantTimeCompare([]byte(token), tokenBytes) != 1 {
			unauthorizedResponse(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// oidcMiddleware validates OIDC tokens against configured trust policies.
// Accepts both Bearer and Basic auth (password = OIDC token), enabling tools like
// pip, Maven, and Bundler that cannot send Bearer headers.
// /health and /metrics are exempt. Other paths require a valid token whose
// matched policy grants permission for the request's protocol.
func (s *Server) oidcMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol := protocolFromPath(r.URL.Path)
		if protocol == "" {
			// Exempt path (health, metrics).
			next.ServeHTTP(w, r)
			return
		}

		telemetry.SetProtocol(r, protocol)

		token, ok := extractToken(r)
		if !ok {
			telemetry.SetAuthOutcome(r, telemetry.AuthOutcomeUnauthorized)
			unauthorizedResponse(w)
			return
		}

		claims, err := s.config.OIDCValidator.ValidateToken(r.Context(), token)
		if err != nil {
			s.logger.Info("OIDC token validation failed", "error", err, "path", r.URL.Path)
			telemetry.SetAuthOutcome(r, telemetry.AuthOutcomeUnauthorized)
			unauthorizedResponse(w)
			return
		}

		if !claims.HasPermission(protocol) {
			s.logger.Info("OIDC insufficient permission", "protocol", protocol, "path", r.URL.Path)
			telemetry.SetAuthOutcome(r, telemetry.AuthOutcomeForbidden)
			forbiddenResponse(w)
			return
		}

		telemetry.SetAuthOutcome(r, telemetry.AuthOutcomeAllowed)
		ctx := auth.WithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// protocolFromPath maps a URL path to a protocol name used in permission checks.
// Returns "" for paths that are exempt from authentication (/health, /metrics).
func protocolFromPath(path string) string {
	switch {
	case path == "/health" || path == "/metrics":
		return ""
	case path == "/stats" || strings.HasPrefix(path, "/admin/"):
		return "admin"
	case strings.HasPrefix(path, "/npm/"):
		return "npm"
	case strings.HasPrefix(path, "/v2/"):
		return "oci"
	case strings.HasPrefix(path, "/pypi/"):
		return "pypi"
	case strings.HasPrefix(path, "/maven/"):
		return "maven"
	case strings.HasPrefix(path, "/rubygems/"):
		return "rubygems"
	case strings.HasPrefix(path, "/git/"):
		return "git"
	case strings.HasPrefix(path, "/goproxy/sumdb/"), strings.HasPrefix(path, "/sumdb/"):
		return "sumdb"
	case strings.HasPrefix(path, "/goproxy/"):
		return "goproxy"
	case strings.HasPrefix(path, "/buildcache/"):
		return "buildcache"
	default:
		return "unknown"
	}
}

// extractToken extracts a token from a request's Authorization header.
// It accepts:
//   - "Bearer <token>" (case-insensitive scheme)
//   - "Basic base64(username:token)" — the password field is treated as the token,
//     enabling tools like pip, Maven, and Bundler that only support Basic auth.
func extractToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		token := authHeader[7:]
		return token, token != ""
	}
	_, password, ok := r.BasicAuth()
	return password, ok && password != ""
}

func forbiddenResponse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"}) //nolint:errcheck
}

func unauthorizedResponse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer, Basic realm="content-cache"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"}) //nolint:errcheck
}
