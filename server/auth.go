package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wolfeidau/content-cache/auth"
)

// authMiddleware returns middleware that validates Bearer token authentication.
// When AuthToken is empty, the middleware is a no-op (allows unauthenticated access).
// Exact paths /health and /metrics are exempt from authentication.
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

		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			unauthorizedResponse(w)
			return
		}

		provided := []byte(strings.TrimPrefix(authHeader, "Bearer "))
		if subtle.ConstantTimeCompare(provided, tokenBytes) != 1 {
			unauthorizedResponse(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// oidcMiddleware validates OIDC Bearer tokens against configured trust policies.
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

		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			unauthorizedResponse(w)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")

		claims, err := s.config.OIDCValidator.ValidateToken(r.Context(), token)
		if err != nil {
			s.logger.Debug("OIDC token validation failed", "error", err, "path", r.URL.Path)
			unauthorizedResponse(w)
			return
		}

		if !claims.HasPermission(protocol) {
			forbiddenResponse(w)
			return
		}

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

func forbiddenResponse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"}) //nolint:errcheck
}

func unauthorizedResponse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"}) //nolint:errcheck
}
