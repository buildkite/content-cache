// Package auth provides OIDC token validation and trust policy enforcement.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

// TrustPolicy defines which OIDC tokens to trust and what they can access.
type TrustPolicy struct {
	Name           string         `json:"name"`
	Issuer         string         `json:"issuer"`
	Audience       []string       `json:"audience,omitempty"`
	RequiredClaims map[string]any `json:"required_claims"`
	// Permissions lists the protocol names this policy grants access to.
	// Use "*" to allow all protocols.
	// Valid names: goproxy, npm, oci, pypi, maven, rubygems, git, sumdb, buildcache, admin
	Permissions []string `json:"permissions"`
}

// ValidatedClaims contains OIDC claims extracted from a verified token.
type ValidatedClaims struct {
	Issuer          string
	Subject         string
	Audience        []string
	Repository      string
	RepositoryOwner string
	Pipeline        string
	Ref             string
	JobID           string
	MatchedPolicy   *TrustPolicy   // the policy that authorized this token
	Raw             map[string]any // all raw claims from the token
}

// HasPermission reports whether the matched policy grants access to the given protocol.
func (c *ValidatedClaims) HasPermission(protocol string) bool {
	if c.MatchedPolicy == nil {
		return false
	}
	return slices.Contains(c.MatchedPolicy.Permissions, "*") ||
		slices.Contains(c.MatchedPolicy.Permissions, protocol)
}

// OIDCValidator validates OIDC tokens against configured trust policies.
type OIDCValidator struct {
	providers map[string]*oidc.Provider
	policies  []TrustPolicy
	logger    *slog.Logger
}

// NewOIDCValidator creates a new validator, initializing an OIDC provider for
// each unique issuer in the policies. Returns an error if any provider cannot
// be reached.
func NewOIDCValidator(ctx context.Context, policies []TrustPolicy, logger *slog.Logger) (*OIDCValidator, error) {
	v := &OIDCValidator{
		providers: make(map[string]*oidc.Provider),
		policies:  policies,
		logger:    logger,
	}

	for _, policy := range policies {
		if len(policy.Audience) == 0 {
			logger.Warn("OIDC policy has no audience constraint — any token from this issuer will pass audience validation",
				"policy", policy.Name, "issuer", policy.Issuer)
		}
		if _, exists := v.providers[policy.Issuer]; !exists {
			provider, err := oidc.NewProvider(ctx, policy.Issuer)
			if err != nil {
				return nil, fmt.Errorf("failed to create OIDC provider for %s: %w", policy.Issuer, err)
			}
			v.providers[policy.Issuer] = provider
			logger.Info("OIDC provider initialized", "issuer", policy.Issuer)
		}
	}

	return v, nil
}

// ValidateToken verifies an OIDC Bearer token and checks it against trust
// policies. Returns validated claims including the matched policy on success.
func (v *OIDCValidator) ValidateToken(ctx context.Context, token string) (*ValidatedClaims, error) {
	// Parse without verification to extract the issuer claim.
	unverified, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	mapClaims, ok := unverified.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("invalid token claims")
	}

	issuer, ok := mapClaims["iss"].(string)
	if !ok {
		return nil, fmt.Errorf("missing issuer claim")
	}

	provider, exists := v.providers[issuer]
	if !exists {
		return nil, fmt.Errorf("untrusted issuer: %s", issuer)
	}

	// Collect allowed audiences from all policies matching this issuer.
	var allowedAudiences []string
	for _, p := range v.policies {
		if p.Issuer == issuer {
			allowedAudiences = append(allowedAudiences, p.Audience...)
		}
	}

	// Verify the token signature via the OIDC provider.
	var idToken *oidc.IDToken
	var lastErr error

	if len(allowedAudiences) == 0 {
		verifier := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
		idToken, lastErr = verifier.Verify(ctx, token)
	} else {
		for _, aud := range allowedAudiences {
			verifier := provider.Verifier(&oidc.Config{ClientID: aud})
			idToken, lastErr = verifier.Verify(ctx, token)
			if lastErr == nil {
				break
			}
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("failed to verify token: %w", lastErr)
	}

	var rawClaims map[string]any
	if err := idToken.Claims(&rawClaims); err != nil {
		return nil, fmt.Errorf("failed to extract claims: %w", err)
	}

	claims := &ValidatedClaims{
		Issuer:   issuer,
		Subject:  idToken.Subject,
		Audience: idToken.Audience,
		Raw:      rawClaims,
	}

	// Extract common CI/CD claims.
	if s, ok := rawClaims["repository"].(string); ok {
		claims.Repository = s
	}

	// Repository owner: GitHub → repository_owner, Buildkite → organization_slug,
	// GitLab → namespace_path, fallback → parse from repository (owner/repo).
	if s, ok := rawClaims["repository_owner"].(string); ok {
		claims.RepositoryOwner = s
	} else if s, ok := rawClaims["organization_slug"].(string); ok {
		claims.RepositoryOwner = s
	} else if s, ok := rawClaims["namespace_path"].(string); ok {
		claims.RepositoryOwner = s
	} else if claims.Repository != "" {
		if parts := strings.SplitN(claims.Repository, "/", 2); len(parts) == 2 {
			claims.RepositoryOwner = parts[0]
		}
	}

	if s, ok := rawClaims["pipeline_slug"].(string); ok {
		claims.Pipeline = s
	}
	if s, ok := rawClaims["ref"].(string); ok {
		claims.Ref = s
	}
	// JobID: GitHub Actions uses job_workflow_ref, Buildkite uses job_id.
	if s, ok := rawClaims["job_workflow_ref"].(string); ok {
		claims.JobID = s
	} else if s, ok := rawClaims["job_id"].(string); ok {
		claims.JobID = s
	}

	if err := v.checkTrustPolicies(claims); err != nil {
		return nil, fmt.Errorf("token does not match any trust policy: %w", err)
	}

	v.logger.Info("OIDC token validated",
		"issuer", claims.Issuer,
		"subject", claims.Subject,
		"repositoryOwner", claims.RepositoryOwner,
		"policy", claims.MatchedPolicy.Name,
	)

	return claims, nil
}

// checkTrustPolicies finds the first policy that matches the claims and sets
// claims.MatchedPolicy. Returns an error if no policy matches.
func (v *OIDCValidator) checkTrustPolicies(claims *ValidatedClaims) error {
	for i := range v.policies {
		policy := &v.policies[i]

		if policy.Issuer != claims.Issuer {
			continue
		}

		// Audience check: at least one policy audience must appear in the token.
		if len(policy.Audience) > 0 {
			matched := false
			for _, pAud := range policy.Audience {
				if slices.Contains(claims.Audience, pAud) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// All required claims must match.
		allMatch := true
		for key, expected := range policy.RequiredClaims {
			actual, exists := claims.Raw[key]
			if !exists || !claimMatches(actual, expected) {
				allMatch = false
				break
			}
		}

		if allMatch {
			v.logger.Debug("token matched trust policy", "policy", policy.Name)
			claims.MatchedPolicy = policy
			return nil
		}
	}

	return fmt.Errorf("no matching trust policy found")
}

// claimMatches checks whether actual satisfies expected.
// Strings support a trailing "*" wildcard. Lists match if any element matches.
func claimMatches(actual, expected any) bool {
	actualStr := fmt.Sprintf("%v", actual)

	switch exp := expected.(type) {
	case string:
		if strings.HasSuffix(exp, "*") {
			return strings.HasPrefix(actualStr, strings.TrimSuffix(exp, "*"))
		}
		return actualStr == exp
	case []any:
		for _, e := range exp {
			if claimMatches(actual, e) {
				return true
			}
		}
		return false
	default:
		return actualStr == fmt.Sprintf("%v", expected)
	}
}

type contextKey string

const claimsContextKey contextKey = "oidc_claims"

// WithClaims stores validated OIDC claims in the context.
func WithClaims(ctx context.Context, claims *ValidatedClaims) context.Context {
	return context.WithValue(ctx, claimsContextKey, claims)
}

// GetClaims retrieves validated OIDC claims from the context.
// Returns false if no claims are present.
func GetClaims(ctx context.Context) (*ValidatedClaims, bool) {
	claims, ok := ctx.Value(claimsContextKey).(*ValidatedClaims)
	return claims, ok
}

// LoadPoliciesFromFile reads trust policies from a JSON file.
// The file must contain a top-level "trust_policies" array.
func LoadPoliciesFromFile(filename string) ([]TrustPolicy, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read policies file: %w", err)
	}

	var config struct {
		TrustPolicies []TrustPolicy `json:"trust_policies"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse policies file: %w", err)
	}

	if len(config.TrustPolicies) == 0 {
		return nil, fmt.Errorf("policies file contains no trust_policies entries")
	}

	return config.TrustPolicies, nil
}
