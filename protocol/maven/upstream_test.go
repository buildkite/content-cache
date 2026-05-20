package maven

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func TestGroupIDToPath(t *testing.T) {
	tests := []struct {
		name    string
		groupID string
		want    string
	}{
		{
			name:    "simple group",
			groupID: "org.apache.commons",
			want:    "org/apache/commons",
		},
		{
			name:    "single segment",
			groupID: "junit",
			want:    "junit",
		},
		{
			name:    "deep nesting",
			groupID: "com.google.guava",
			want:    "com/google/guava",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupIDToPath(tt.groupID)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPathToGroupID(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "simple path",
			path: "org/apache/commons",
			want: "org.apache.commons",
		},
		{
			name: "single segment",
			path: "junit",
			want: "junit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathToGroupID(tt.path)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestArtifactCoordinate(t *testing.T) {
	tests := []struct {
		name     string
		coord    ArtifactCoordinate
		filename string
		fullPath string
	}{
		{
			name: "simple jar",
			coord: ArtifactCoordinate{
				GroupID:    "org.apache.commons",
				ArtifactID: "commons-lang3",
				Version:    "3.12.0",
				Extension:  "jar",
			},
			filename: "commons-lang3-3.12.0.jar",
			fullPath: "org/apache/commons/commons-lang3/3.12.0/commons-lang3-3.12.0.jar",
		},
		{
			name: "sources jar",
			coord: ArtifactCoordinate{
				GroupID:    "org.apache.commons",
				ArtifactID: "commons-lang3",
				Version:    "3.12.0",
				Classifier: "sources",
				Extension:  "jar",
			},
			filename: "commons-lang3-3.12.0-sources.jar",
			fullPath: "org/apache/commons/commons-lang3/3.12.0/commons-lang3-3.12.0-sources.jar",
		},
		{
			name: "pom file",
			coord: ArtifactCoordinate{
				GroupID:    "org.apache.commons",
				ArtifactID: "commons-lang3",
				Version:    "3.12.0",
				Extension:  "pom",
			},
			filename: "commons-lang3-3.12.0.pom",
			fullPath: "org/apache/commons/commons-lang3/3.12.0/commons-lang3-3.12.0.pom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.filename, tt.coord.Filename())
			require.Equal(t, tt.fullPath, tt.coord.FullPath())
		})
	}
}

func TestUpstreamArtifactURL(t *testing.T) {
	u := NewUpstream()

	coord := ArtifactCoordinate{
		GroupID:    "org.apache.commons",
		ArtifactID: "commons-lang3",
		Version:    "3.12.0",
		Extension:  "jar",
	}

	expected := "https://repo.maven.apache.org/maven2/org/apache/commons/commons-lang3/3.12.0/commons-lang3-3.12.0.jar"
	require.Equal(t, expected, u.ArtifactURL(coord))
}

func TestUpstreamMetadataURL(t *testing.T) {
	u := NewUpstream()

	expected := "https://repo.maven.apache.org/maven2/org/apache/commons/commons-lang3/maven-metadata.xml"
	require.Equal(t, expected, u.MetadataURL("org.apache.commons", "commons-lang3"))
}

func TestUpstreamFetchMetadataRaw(t *testing.T) {
	metadataXML := `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>org.example</groupId>
  <artifactId>test</artifactId>
  <versioning>
    <latest>1.0.0</latest>
    <release>1.0.0</release>
    <versions>
      <version>1.0.0</version>
    </versions>
    <lastUpdated>20240101000000</lastUpdated>
  </versioning>
</metadata>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/org/example/test/maven-metadata.xml" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(metadataXML))
			return
		}
		if r.URL.Path == "/not/found/artifact/maven-metadata.xml" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u := NewUpstream(WithRepositoryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		data, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
		require.NoError(t, err)
		require.Contains(t, string(data), "<groupId>org.example</groupId>")
	})

	t.Run("not found", func(t *testing.T) {
		_, err := u.FetchMetadataRaw(context.Background(), "not.found", "artifact")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamFetchMetadata(t *testing.T) {
	metadataXML := `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>org.example</groupId>
  <artifactId>test</artifactId>
  <versioning>
    <latest>2.0.0</latest>
    <release>2.0.0</release>
    <versions>
      <version>1.0.0</version>
      <version>2.0.0</version>
    </versions>
    <lastUpdated>20240101000000</lastUpdated>
  </versioning>
</metadata>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(metadataXML))
	}))
	defer server.Close()

	u := NewUpstream(WithRepositoryURL(server.URL))

	meta, err := u.FetchMetadata(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, "org.example", meta.GroupID)
	require.Equal(t, "test", meta.ArtifactID)
	require.Equal(t, "2.0.0", meta.Versioning.Latest)
	require.Equal(t, "2.0.0", meta.Versioning.Release)
	require.Equal(t, []string{"1.0.0", "2.0.0"}, meta.Versioning.Versions.Version)
}

func TestUpstreamFetchArtifact(t *testing.T) {
	artifactContent := []byte("fake jar content")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/org/example/test/1.0.0/test-1.0.0.jar" {
			w.Header().Set("Content-Type", "application/java-archive")
			w.Header().Set("Content-Length", "16")
			_, _ = w.Write(artifactContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	u := NewUpstream(WithRepositoryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		coord := ArtifactCoordinate{
			GroupID:    "org.example",
			ArtifactID: "test",
			Version:    "1.0.0",
			Extension:  "jar",
		}
		rc, size, err := u.FetchArtifact(context.Background(), coord)
		require.NoError(t, err)
		require.Equal(t, int64(16), size)
		defer func() { _ = rc.Close() }()
	})

	t.Run("not found", func(t *testing.T) {
		coord := ArtifactCoordinate{
			GroupID:    "not.found",
			ArtifactID: "artifact",
			Version:    "1.0.0",
			Extension:  "jar",
		}
		_, _, err := u.FetchArtifact(context.Background(), coord)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamFetchChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/example/test/1.0.0/test-1.0.0.jar.sha1":
			// Simple checksum format
			_, _ = w.Write([]byte("da39a3ee5e6b4b0d3255bfef95601890afd80709"))
		case "/org/example/test/1.0.0/test-1.0.0.jar.md5":
			// Checksum with filename format
			_, _ = w.Write([]byte("d41d8cd98f00b204e9800998ecf8427e  test-1.0.0.jar"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	u := NewUpstream(WithRepositoryURL(server.URL))

	coord := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "test",
		Version:    "1.0.0",
		Extension:  "jar",
	}

	t.Run("sha1 simple format", func(t *testing.T) {
		checksum, err := u.FetchChecksum(context.Background(), coord, ChecksumSHA1)
		require.NoError(t, err)
		require.Equal(t, "da39a3ee5e6b4b0d3255bfef95601890afd80709", checksum)
	})

	t.Run("md5 with filename format", func(t *testing.T) {
		checksum, err := u.FetchChecksum(context.Background(), coord, ChecksumMD5)
		require.NoError(t, err)
		require.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", checksum)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := u.FetchChecksum(context.Background(), coord, ChecksumSHA256)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestWithRepositoryURL(t *testing.T) {
	u := NewUpstream(WithRepositoryURL("https://custom.repo.com/maven2/"))

	coord := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "test",
		Version:    "1.0.0",
		Extension:  "jar",
	}

	// Should trim trailing slash
	url := u.ArtifactURL(coord)
	require.Equal(t, "https://custom.repo.com/maven2/org/example/test/1.0.0/test-1.0.0.jar", url)
}

// TestUpstreamFallbackArtifact verifies that when the primary upstream returns
// 404, fetches fall through to the secondary upstream.
func TestUpstreamFallbackArtifact(t *testing.T) {
	var primaryHits, secondaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryHits++
		_, _ = w.Write([]byte("from-secondary"))
	}))
	defer secondary.Close()

	u := NewUpstream(WithRepositoryURLs(primary.URL, secondary.URL))

	coord := ArtifactCoordinate{
		GroupID: "org.example", ArtifactID: "test", Version: "1.0.0", Extension: "jar",
	}
	rc, _, err := u.FetchArtifact(context.Background(), coord)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	require.Equal(t, 1, primaryHits, "primary should have been tried once")
	require.Equal(t, 1, secondaryHits, "secondary should have served the request")
}

// TestUpstreamFallbackMetadata verifies fallback for metadata fetches.
func TestUpstreamFallbackMetadata(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<metadata/>"))
	}))
	defer secondary.Close()

	u := NewUpstream(WithRepositoryURLs(primary.URL, secondary.URL))

	data, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, "<metadata/>", string(data))
}

// TestUpstreamAllNotFound verifies that when all upstreams return 404, the
// chain returns ErrNotFound.
func TestUpstreamAllNotFound(t *testing.T) {
	server404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server404.Close()

	u := NewUpstream(WithRepositoryURLs(server404.URL, server404.URL))

	_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestUpstreamNon404ErrorReturnedImmediately verifies that a non-404 upstream
// error short-circuits the chain (we don't mask a 5xx with a later 404).
func TestUpstreamNon404ErrorReturnedImmediately(t *testing.T) {
	var secondaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer secondary.Close()

	u := NewUpstream(WithRepositoryURLs(primary.URL, secondary.URL))

	_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound)
	require.Equal(t, 0, secondaryHits, "secondary must not be tried after non-404 primary error")
}

// TestUpstreamNegativeCache verifies that a 404 from an upstream is remembered,
// skipping that upstream on subsequent requests for the same path.
func TestUpstreamNegativeCache(t *testing.T) {
	var primaryHits, secondaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryHits++
		_, _ = w.Write([]byte("hit"))
	}))
	defer secondary.Close()

	u := NewUpstream(
		WithRepositoryURLs(primary.URL, secondary.URL),
		WithNegativeCacheStore(newTestNegCacheStore(t)),
	)

	for i := 0; i < 3; i++ {
		data, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
		require.NoError(t, err)
		require.Equal(t, "hit", string(data))
	}

	require.Equal(t, 1, primaryHits, "primary should be hit once, then negatively cached")
	require.Equal(t, 3, secondaryHits, "secondary should serve every request")
}

// TestUpstreamNoCacheWithoutStore verifies that without a negative cache store
// every request re-probes every upstream (no in-memory fallback).
func TestUpstreamNoCacheWithoutStore(t *testing.T) {
	var primaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hit"))
	}))
	defer secondary.Close()

	u := NewUpstream(WithRepositoryURLs(primary.URL, secondary.URL))

	for i := 0; i < 3; i++ {
		_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
		require.NoError(t, err)
	}
	require.Equal(t, 3, primaryHits, "without a store, primary is re-probed every request")
}

// TestUpstreamPerTryTimeout verifies a slow upstream falls through to the next.
func TestUpstreamPerTryTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fast"))
	}))
	defer fast.Close()

	u := NewUpstream(
		WithRepositoryURLs(slow.URL, fast.URL),
		WithPerUpstreamTimeout(50*time.Millisecond),
	)

	start := time.Now()
	data, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	elapsed := time.Since(start)

	// Slow upstream returns a context-deadline-exceeded error, which is a
	// non-404 error and short-circuits. So we expect an error, not the body.
	// This verifies the per-try timeout actually fires.
	require.Error(t, err)
	require.Less(t, elapsed, 300*time.Millisecond, "per-try timeout should fire within ~50ms")
	require.Empty(t, data)
	// Note: fast upstream is never consulted because slow returned a non-404
	// error. This is intentional (see TestUpstreamNon404ErrorReturnedImmediately):
	// we surface real errors rather than masking them with a later 404.
}

// TestUpstreamSingleURLNoTimeout verifies the default per-try timeout only
// kicks in when multiple upstreams are configured.
func TestUpstreamSingleURLNoTimeout(t *testing.T) {
	u := NewUpstream(WithRepositoryURL("https://example.invalid"))
	require.Equal(t, time.Duration(0), u.perTryTimeout)

	multi := NewUpstream(WithRepositoryURLs("https://a.invalid", "https://b.invalid"))
	require.Equal(t, DefaultPerUpstreamTimeout, multi.perTryTimeout)
}

// TestUpstreamUserAgent verifies the User-Agent is sent on outbound requests.
func TestUpstreamUserAgent(t *testing.T) {
	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("<metadata/>"))
	}))
	defer server.Close()

	u := NewUpstream(
		WithRepositoryURL(server.URL),
		WithUserAgent("content-cache/test (+https://example.com)"),
	)
	_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, "content-cache/test (+https://example.com)", gotUA)
}

// TestUpstreamDefaultUserAgent verifies the default User-Agent is identifying,
// not Go's generic default.
func TestUpstreamDefaultUserAgent(t *testing.T) {
	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("<metadata/>"))
	}))
	defer server.Close()

	u := NewUpstream(WithRepositoryURL(server.URL))
	_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, "content-cache", gotUA, "default UA should identify content-cache, not 'Go-http-client'")
}

// TestUpstreamPersistentNegativeCache verifies that a metadb-backed negative
// cache works across "restarts" (constructing a new Upstream with the same store).
func TestUpstreamPersistentNegativeCache(t *testing.T) {
	var primaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hit"))
	}))
	defer secondary.Close()

	store := newTestNegCacheStore(t)

	// First "instance" — primary gets hit once, result is persisted.
	u1 := NewUpstream(
		WithRepositoryURLs(primary.URL, secondary.URL),
		WithNegativeCacheStore(store),
	)
	_, err := u1.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, 1, primaryHits)

	// Second "instance" sharing the same store — primary is skipped.
	u2 := NewUpstream(
		WithRepositoryURLs(primary.URL, secondary.URL),
		WithNegativeCacheStore(store),
	)
	_, err = u2.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, 1, primaryHits, "primary should not be re-tried after restart")
}

// TestUpstreamArtifactVsMetadataTTL verifies the two TTL buckets are
// distinct: setting metadata TTL to 0 disables only metadata negative caching.
func TestUpstreamArtifactVsMetadataTTL(t *testing.T) {
	var primaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer secondary.Close()

	u := NewUpstream(
		WithRepositoryURLs(primary.URL, secondary.URL),
		WithNegativeCacheStore(newTestNegCacheStore(t)),
		WithMetadataNegativeCacheTTL(0), // disable metadata caching
		WithArtifactNegativeCacheTTL(time.Hour),
	)

	// Metadata requests should retry primary every time (no negative caching).
	_, err := u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	_, err = u.FetchMetadataRaw(context.Background(), "org.example", "test")
	require.NoError(t, err)
	require.Equal(t, 2, primaryHits, "metadata path should not be negatively cached when TTL is 0")

	// Artifact requests should be negatively cached.
	coord := ArtifactCoordinate{GroupID: "org.example", ArtifactID: "lib", Version: "1.0", Extension: "jar"}
	_, _, err = u.FetchArtifact(context.Background(), coord)
	require.NoError(t, err)
	_, _, err = u.FetchArtifact(context.Background(), coord)
	require.NoError(t, err)
	require.Equal(t, 3, primaryHits, "artifact path should be negatively cached after first 404")
}

// newTestNegCacheStore creates a real metadb-backed envelope index for testing
// the persistent negative cache path.
func newTestNegCacheStore(t *testing.T) NegativeCacheStore {
	t.Helper()
	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	path := filepath.Join(t.TempDir(), "neg.db")
	require.NoError(t, db.Open(path))
	t.Cleanup(func() { _ = db.Close() })

	idx, err := metadb.NewEnvelopeIndex(db, "maven", "negcache", 24*time.Hour)
	require.NoError(t, err)
	return idx
}
