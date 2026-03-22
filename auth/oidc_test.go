package auth

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClaimMatches(t *testing.T) {
	tests := []struct {
		name     string
		actual   any
		expected any
		want     bool
	}{
		{"exact string match", "myorg", "myorg", true},
		{"exact string no match", "myorg", "other", false},
		{"wildcard prefix match", "refs/heads/main", "refs/heads/*", true},
		{"wildcard prefix no match", "refs/tags/v1", "refs/heads/*", false},
		{"wildcard star only", "anything", "*", true},
		{"list match first", "npm", []any{"npm", "goproxy"}, true},
		{"list match second", "goproxy", []any{"npm", "goproxy"}, true},
		{"list no match", "oci", []any{"npm", "goproxy"}, false},
		{"non-string type", 42, 42, true},
		{"non-string type no match", 42, 43, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, claimMatches(tc.actual, tc.expected))
		})
	}
}

func TestHasPermission(t *testing.T) {
	claims := &ValidatedClaims{
		MatchedPolicy: &TrustPolicy{
			Permissions: []string{"goproxy", "buildcache"},
		},
	}

	require.True(t, claims.HasPermission("goproxy"))
	require.True(t, claims.HasPermission("buildcache"))
	require.False(t, claims.HasPermission("npm"))
	require.False(t, claims.HasPermission("oci"))
}

func TestHasPermissionWildcard(t *testing.T) {
	claims := &ValidatedClaims{
		MatchedPolicy: &TrustPolicy{
			Permissions: []string{"*"},
		},
	}

	require.True(t, claims.HasPermission("goproxy"))
	require.True(t, claims.HasPermission("npm"))
	require.True(t, claims.HasPermission("buildcache"))
}

func TestHasPermissionNilPolicy(t *testing.T) {
	claims := &ValidatedClaims{}
	require.False(t, claims.HasPermission("goproxy"))
}

func TestCheckTrustPolicies(t *testing.T) {
	validator := &OIDCValidator{
		logger: discardLogger(),
		policies: []TrustPolicy{
			{
				Name:     "buildkite-myorg",
				Issuer:   "https://agent.buildkite.com",
				Audience: []string{"https://cache.example.com"},
				RequiredClaims: map[string]any{
					"organization_slug": "myorg",
				},
				Permissions: []string{"goproxy", "buildcache"},
			},
		},
	}

	t.Run("matching policy", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer:   "https://agent.buildkite.com",
			Audience: []string{"https://cache.example.com"},
			Raw: map[string]any{
				"organization_slug": "myorg",
			},
		}
		err := validator.checkTrustPolicies(claims)
		require.NoError(t, err)
		require.NotNil(t, claims.MatchedPolicy)
		require.Equal(t, "buildkite-myorg", claims.MatchedPolicy.Name)
	})

	t.Run("wrong issuer", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer:   "https://other.example.com",
			Audience: []string{"https://cache.example.com"},
			Raw:      map[string]any{"organization_slug": "myorg"},
		}
		err := validator.checkTrustPolicies(claims)
		require.Error(t, err)
	})

	t.Run("wrong audience", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer:   "https://agent.buildkite.com",
			Audience: []string{"https://other.example.com"},
			Raw:      map[string]any{"organization_slug": "myorg"},
		}
		err := validator.checkTrustPolicies(claims)
		require.Error(t, err)
	})

	t.Run("missing required claim", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer:   "https://agent.buildkite.com",
			Audience: []string{"https://cache.example.com"},
			Raw:      map[string]any{},
		}
		err := validator.checkTrustPolicies(claims)
		require.Error(t, err)
	})

	t.Run("wrong required claim value", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer:   "https://agent.buildkite.com",
			Audience: []string{"https://cache.example.com"},
			Raw:      map[string]any{"organization_slug": "otherorg"},
		}
		err := validator.checkTrustPolicies(claims)
		require.Error(t, err)
	})
}

func TestCheckTrustPoliciesWildcardClaim(t *testing.T) {
	validator := &OIDCValidator{
		logger: discardLogger(),
		policies: []TrustPolicy{
			{
				Name:   "github-main-branch",
				Issuer: "https://token.actions.githubusercontent.com",
				RequiredClaims: map[string]any{
					"ref": "refs/heads/*",
				},
				Permissions: []string{"goproxy"},
			},
		},
	}

	t.Run("matches wildcard branch", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer: "https://token.actions.githubusercontent.com",
			Raw:    map[string]any{"ref": "refs/heads/main"},
		}
		require.NoError(t, validator.checkTrustPolicies(claims))
	})

	t.Run("does not match tag ref", func(t *testing.T) {
		claims := &ValidatedClaims{
			Issuer: "https://token.actions.githubusercontent.com",
			Raw:    map[string]any{"ref": "refs/tags/v1.0.0"},
		}
		require.Error(t, validator.checkTrustPolicies(claims))
	})
}

func TestLoadPoliciesFromFile(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		policies := []TrustPolicy{
			{
				Name:           "test-policy",
				Issuer:         "https://agent.buildkite.com",
				Audience:       []string{"https://cache.example.com"},
				RequiredClaims: map[string]any{"organization_slug": "myorg"},
				Permissions:    []string{"goproxy"},
			},
		}
		data, _ := json.Marshal(map[string]any{"trust_policies": policies})

		f := filepath.Join(t.TempDir(), "policies.json")
		require.NoError(t, os.WriteFile(f, data, 0600))

		loaded, err := LoadPoliciesFromFile(f)
		require.NoError(t, err)
		require.Len(t, loaded, 1)
		require.Equal(t, "test-policy", loaded[0].Name)
		require.Equal(t, []string{"goproxy"}, loaded[0].Permissions)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(f, []byte("{bad json}"), 0600))

		_, err := LoadPoliciesFromFile(f)
		require.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadPoliciesFromFile("/nonexistent/policies.json")
		require.Error(t, err)
	})

	t.Run("empty trust_policies array", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "empty.json")
		require.NoError(t, os.WriteFile(f, []byte(`{"trust_policies":[]}`), 0600))

		_, err := LoadPoliciesFromFile(f)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no trust_policies")
	})
}
