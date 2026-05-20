package maven

import (
	"context"
	"net/http"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
)

const (
	// DefaultArtifactNegativeCacheTTL is how long a per-upstream 404 for an
	// immutable artifact path is remembered. Long: artifact coordinates are
	// immutable (a new version is a new path), so stale negative cache is
	// harmless. Reduces repeat 404 probes that trigger upstream rate limiting
	// and IP blocklisting (e.g. Sonatype's "abusive client" heuristic).
	DefaultArtifactNegativeCacheTTL = 24 * time.Hour

	// DefaultMetadataNegativeCacheTTL is how long a per-upstream 404 for a
	// mutable metadata path (maven-metadata.xml, root catalogs) is remembered.
	// Short, so newly published versions become discoverable quickly.
	DefaultMetadataNegativeCacheTTL = 1 * time.Minute
)

// NegativeCacheStore persists per-(upstream, path) 404 markers with TTL.
// *metadb.EnvelopeIndex satisfies this interface.
type NegativeCacheStore interface {
	PutNegativeCache(ctx context.Context, key string, statusCode uint32, ttl time.Duration) error
	GetEnvelope(ctx context.Context, key string) (*metadb.MetadataEnvelope, error)
}

// pathKind selects the negative-cache TTL bucket for a path.
type pathKind int

const (
	// pathKindArtifact is for immutable artifact paths (jar/pom/checksums).
	pathKindArtifact pathKind = iota
	// pathKindMetadata is for mutable paths (maven-metadata.xml, root catalogs).
	pathKindMetadata
)

// WithArtifactNegativeCacheTTL sets how long a per-upstream 404 for an
// immutable artifact path is remembered. Zero disables artifact negative caching.
func WithArtifactNegativeCacheTTL(d time.Duration) UpstreamOption {
	return func(u *Upstream) {
		u.artifactNegTTL = d
	}
}

// WithMetadataNegativeCacheTTL sets how long a per-upstream 404 for a mutable
// metadata path is remembered. Zero disables metadata negative caching.
func WithMetadataNegativeCacheTTL(d time.Duration) UpstreamOption {
	return func(u *Upstream) {
		u.metadataNegTTL = d
	}
}

// WithNegativeCacheStore enables persistent negative caching. Without a store,
// every miss re-probes every upstream on every request — fine for tests,
// dangerous in production against rate-limiting upstreams like Maven Central.
func WithNegativeCacheStore(s NegativeCacheStore) UpstreamOption {
	return func(u *Upstream) {
		u.negCacheStore = s
	}
}

func (u *Upstream) ttlFor(kind pathKind) time.Duration {
	if kind == pathKindMetadata {
		return u.metadataNegTTL
	}
	return u.artifactNegTTL
}

// negCacheKey is the persistent storage key for a (upstream-url, path) pair.
// Keyed by URL (not list index) so reordering, inserting, or removing entries
// in --maven-upstream doesn't reassign cached 404s to a different repository.
// Orphan keys from removed upstreams expire via their TTL.
//
// NUL is used as the field separator because it cannot appear in a URL or
// a Maven path, so two distinct (baseURL, key) pairs can never collide.
func negCacheKey(baseURL, key string) string {
	return baseURL + "\x00" + key
}

func (u *Upstream) isNegativelyCached(ctx context.Context, baseURL, key string) bool {
	if u.negCacheStore == nil {
		return false
	}
	env, err := u.negCacheStore.GetEnvelope(ctx, negCacheKey(baseURL, key))
	if err != nil {
		return false // treat any error (incl. ErrNotFound) as cache miss
	}
	if metadb.IsExpired(env) {
		return false // reaper will lazily clean up
	}
	return metadb.IsNegativeCache(env)
}

func (u *Upstream) markNegative(ctx context.Context, kind pathKind, baseURL, key string) {
	if u.negCacheStore == nil {
		return
	}
	ttl := u.ttlFor(kind)
	if ttl <= 0 {
		return
	}
	// Best-effort: a persistence failure must not break the request path.
	_ = u.negCacheStore.PutNegativeCache(ctx, negCacheKey(baseURL, key), http.StatusNotFound, ttl)
}
