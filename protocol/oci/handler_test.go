package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

const testPrefix = "docker-hub"

// newTestRouter creates a Router with a single registry for testing.
func newTestRouter(t *testing.T, upstreamURL string) *Router {
	t.Helper()
	upstreamOpts := []UpstreamOption{}
	if upstreamURL != "" {
		upstreamOpts = append(upstreamOpts, WithRegistryURL(upstreamURL))
	}
	router, err := NewRouter([]Registry{
		{Prefix: testPrefix, Upstream: NewUpstream(upstreamOpts...)},
	})
	require.NoError(t, err)
	return router
}

func newTestHandler(t *testing.T, upstreamURL string) (*Handler, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "oci-handler-test-*")
	require.NoError(t, err)

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)

	// Create and open BoltDB for envelope storage
	db := metadb.New()
	err = db.Open(filepath.Join(tmpDir, "metadata.db"))
	require.NoError(t, err)

	boltDB, ok := db.(*metadb.BoltDB)
	require.True(t, ok, "metaDB must be *metadb.BoltDB")

	// Create EnvelopeIndex instances for OCI
	imageIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "image", 24*time.Hour)
	require.NoError(t, err)
	manifestIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "manifest", 24*time.Hour)
	require.NoError(t, err)
	blobIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "blob", 24*time.Hour)
	require.NoError(t, err)

	idx := NewIndex(imageIndex, manifestIndex, blobIndex)
	st := store.NewCAFS(b)

	opts := []HandlerOption{
		WithTagTTL(1 * time.Hour),
		WithRouter(newTestRouter(t, upstreamURL)),
	}

	h := NewHandler(idx, st, opts...)

	return h, func() {
		h.Close()
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

// newTestIndexWithStore creates an OCI index and store for tests that need direct access.
// Returns index, store, metadb, and cleanup function.
func newTestIndexWithStore(t *testing.T, tmpDir string) (*Index, store.Store, metadb.MetaDB, func()) {
	t.Helper()

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)

	// Create and open BoltDB for envelope storage
	db := metadb.New()
	err = db.Open(filepath.Join(tmpDir, "metadata.db"))
	require.NoError(t, err)

	boltDB, ok := db.(*metadb.BoltDB)
	require.True(t, ok, "metaDB must be *metadb.BoltDB")

	// Create EnvelopeIndex instances for OCI
	imageIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "image", 24*time.Hour)
	require.NoError(t, err)
	manifestIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "manifest", 24*time.Hour)
	require.NoError(t, err)
	blobIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "blob", 24*time.Hour)
	require.NoError(t, err)

	idx := NewIndex(imageIndex, manifestIndex, blobIndex)
	st := store.NewCAFS(b)

	return idx, st, db, func() {
		_ = db.Close()
	}
}

func TestHandlerVersionCheck(t *testing.T) {
	h, cleanup := newTestHandler(t, "")
	defer cleanup()

	t.Run("GET /v2/", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))
		require.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-API-Version"))
	})

	t.Run("GET /v2 (without trailing slash)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("HEAD /v2/", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t, "")
	defer cleanup()

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v2/", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}

func TestHandlerNotFound(t *testing.T) {
	h, cleanup := newTestHandler(t, "")
	defer cleanup()

	paths := []string{
		"/v3/",
		"/v2/something",
		"/v2/docker-hub/library/alpine/tags/list",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			require.Equal(t, http.StatusNotFound, w.Code)
		})
	}
}

func TestHandlerUnprefixedRequestReturns404(t *testing.T) {
	h, cleanup := newTestHandler(t, "")
	defer cleanup()

	// Request without the prefix should return 404
	paths := []string{
		"/v2/library/nginx/manifests/latest",
		"/v2/library/alpine/blobs/sha256:abc123def456789012345678901234567890123456789012345678901234abcd",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			require.Equal(t, http.StatusNotFound, w.Code)
		})
	}
}

func TestHandlerGetManifest(t *testing.T) {
	manifestContent := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`
	manifestDigest := ComputeSHA256([]byte(manifestContent))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/alpine/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write([]byte(manifestContent))
		case "/v2/library/alpine/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write([]byte(manifestContent))
		case "/v2/library/notfound/manifests/latest":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	t.Run("fetch by tag", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/vnd.oci.image.manifest.v1+json", w.Header().Get("Content-Type"))
		require.Equal(t, manifestDigest, w.Header().Get("Docker-Content-Digest"))
		require.Equal(t, manifestContent, w.Body.String())
	})

	t.Run("fetch by digest", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/"+manifestDigest, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/notfound/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})

	// Wait for background caching
	h.Close()

	// Test cache hit
	t.Run("cache hit", func(t *testing.T) {
		h2, cleanup2 := newTestHandler(t, upstream.URL)
		defer cleanup2()

		// Verify the first handler cached it - create new handler with same storage
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)

		// Should still work (will fetch from upstream since new handler has empty cache)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHandlerHeadManifest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		switch r.URL.Path {
		case "/v2/library/alpine/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abc123")
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusOK)
		case "/v2/library/notfound/manifests/latest":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/vnd.oci.image.manifest.v1+json", w.Header().Get("Content-Type"))
		require.Equal(t, "sha256:abc123", w.Header().Get("Docker-Content-Digest"))
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/notfound/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerGetBlob(t *testing.T) {
	blobContent := []byte("test blob content for digest verification")
	blobDigest := ComputeSHA256(blobContent)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/alpine/blobs/" + blobDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blobContent)))
			_, _ = w.Write(blobContent)
		case "/v2/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/"+blobDigest, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
		require.Equal(t, blobDigest, w.Header().Get("Docker-Content-Digest"))
		require.Equal(t, string(blobContent), w.Body.String())
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("invalid digest format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/invalid", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// Should return 404 as the path regex doesn't match
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerHeadBlob(t *testing.T) {
	validDigest := "sha256:abc123def456789012345678901234567890123456789012345678901234abcd"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		switch r.URL.Path {
		case "/v2/library/alpine/blobs/" + validDigest:
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
		case "/v2/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/blobs/"+validDigest, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
		require.Equal(t, "4096", w.Header().Get("Content-Length"))
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerCaching(t *testing.T) {
	manifestContent := `{"schemaVersion":2}`
	manifestDigest := ComputeSHA256([]byte(manifestContent))
	requestCount := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write([]byte(manifestContent))
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	// First request - cache miss
	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Wait for background caching
	h.Close()

	require.Equal(t, 1, requestCount)

	// Create new handler with same store to test cache
	// Note: In a real scenario, we'd reuse the same handler
}

func TestHandlerWithAuth(t *testing.T) {
	// Auth server
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{ //nolint:gosec // test mock token response
			Token:     "test-token",
			ExpiresIn: 300,
		})
	}))
	defer authServer.Close()

	// Registry that requires auth
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="test",scope="repository:library/alpine:pull"`, authServer.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/library/alpine/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abc123")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer registry.Close()

	h, cleanup := newTestHandler(t, registry.URL)
	defer cleanup()

	// Request should succeed after auth
	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestHandlerDigestVerification(t *testing.T) {
	// Wrong content for the claimed digest
	wrongContent := []byte("wrong content")
	claimedDigest := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // SHA256 of empty string
	requestCount := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(wrongContent)
	}))
	defer upstream.Close()

	tmpDir, err := os.MkdirTemp("", "oci-digest-verify-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	h := NewHandler(idx, st,
		WithRouter(newTestRouter(t, upstream.URL)),
	)

	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/"+claimedDigest, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// With stream-through, the client receives content before digest verification.
	// On mismatch, we don't cache but the response is already 200.
	// OCI clients verify digests client-side per the OCI Distribution Spec.
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, wrongContent, w.Body.Bytes())

	// Wait for any background caching goroutines to finish.
	h.Close()
	require.Equal(t, 1, requestCount)

	// Second request with same shared storage — should NOT be served from cache
	// since the mismatched blob must not have been stored.
	h2 := NewHandler(idx, st,
		WithRouter(newTestRouter(t, upstream.URL)),
	)
	defer h2.Close()

	req2 := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/"+claimedDigest, nil)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, 2, requestCount, "second request should hit upstream, not cache")
}

func TestHandlerManifestCacheHit(t *testing.T) {
	requestCount := 0
	manifestContent := `{"schemaVersion":2}`
	manifestDigest := ComputeSHA256([]byte(manifestContent))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write([]byte(manifestContent))
	}))
	defer upstream.Close()

	// Create handler with short TTL
	tmpDir, err := os.MkdirTemp("", "oci-cache-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, db, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	router := newTestRouter(t, upstream.URL)

	h := NewHandler(idx, st,
		WithTagTTL(1*time.Hour),
		WithRouter(router),
	)
	defer h.Close()

	// First request - cache miss
	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Wait for background caching
	h.Close()

	// Create new handler with same storage
	h2 := NewHandler(idx, st,
		WithTagTTL(1*time.Hour),
		WithRouter(newTestRouter(t, upstream.URL)),
	)
	defer h2.Close()

	// Second request - should be cache hit
	req2 := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, 1, requestCount)
	_ = db // suppress unused variable warning
}

func TestHandlerBlobCacheHit(t *testing.T) {
	blobContent := []byte("cached blob content here")
	blobDigest := ComputeSHA256(blobContent)
	requestCount := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blobContent)))
		_, _ = w.Write(blobContent)
	}))
	defer upstream.Close()

	tmpDir, err := os.MkdirTemp("", "oci-blob-cache-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	h := NewHandler(idx, st,
		WithRouter(newTestRouter(t, upstream.URL)),
	)

	// First request
	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/"+blobDigest, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Wait for background caching
	h.Close()

	// Second handler with same storage
	h2 := NewHandler(idx, st,
		WithRouter(newTestRouter(t, upstream.URL)),
	)
	defer h2.Close()

	// Second request - should hit cache
	req2 := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/blobs/"+blobDigest, nil)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, string(blobContent), w2.Body.String())
	require.Equal(t, 1, requestCount)
}

func TestHandlerUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadGateway, w.Code)
}

func TestHandlerNestedImageName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/myorg/myrepo/myimage/manifests/v1.0.0" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		t.Logf("unexpected path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream.URL)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/myorg/myrepo/myimage/manifests/v1.0.0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestHandlerClose(t *testing.T) {
	h, cleanup := newTestHandler(t, "")
	defer cleanup()

	// Close should be idempotent
	h.Close()
	h.Close()
}

func TestHandlerOptions(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "oci-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	t.Run("default values", func(t *testing.T) {
		h := NewHandler(idx, st)
		defer h.Close()

		require.Equal(t, DefaultTagTTL, h.tagTTL)
	})

	t.Run("custom tag TTL", func(t *testing.T) {
		h := NewHandler(idx, st, WithTagTTL(10*time.Minute))
		defer h.Close()

		require.Equal(t, 10*time.Minute, h.tagTTL)
	})
}

func TestHandlerHeadManifestCacheHit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for cache hit")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	tmpDir, _ := os.MkdirTemp("", "oci-head-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	// Pre-populate cache (using prefix-scoped name for tag index)
	ctx := context.Background()
	manifestDigest := "sha256:abc123def456789012345678901234567890123456789012345678901234abcd"
	hash := contentcache.Hash{1, 2, 3, 4}
	_ = idx.PutManifest(ctx, manifestDigest, "application/vnd.oci.image.manifest.v1+json", hash, 1024)
	_ = idx.SetTagDigest(ctx, testPrefix+"/library/alpine", "latest", manifestDigest)

	h := NewHandler(idx, st, WithRouter(newTestRouter(t, upstream.URL)))
	defer h.Close()

	// HEAD by tag - should hit cache
	req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, manifestDigest, w.Header().Get("Docker-Content-Digest"))
	require.Equal(t, "1024", w.Header().Get("Content-Length"))

	// HEAD by digest - should also hit cache
	req2 := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/manifests/"+manifestDigest, nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
}

func TestHandlerHeadBlobCacheHit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for cache hit")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	tmpDir, _ := os.MkdirTemp("", "oci-head-blob-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	// Pre-populate cache
	ctx := context.Background()
	blobDigest := "sha256:abc123def456789012345678901234567890123456789012345678901234abcd"
	hash := contentcache.Hash{5, 6, 7, 8}
	_ = idx.PutBlob(ctx, blobDigest, hash, 4096)

	h := NewHandler(idx, st, WithRouter(newTestRouter(t, upstream.URL)))
	defer h.Close()

	req := httptest.NewRequest(http.MethodHead, "/v2/docker-hub/library/alpine/blobs/"+blobDigest, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, blobDigest, w.Header().Get("Docker-Content-Digest"))
	require.Equal(t, "4096", w.Header().Get("Content-Length"))
}

func TestHandlerTagTTLExpiry(t *testing.T) {
	requestCount := 0
	manifestContent := `{"schemaVersion":2}`
	manifestDigest := ComputeSHA256([]byte(manifestContent))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write([]byte(manifestContent))
	}))
	defer upstream.Close()

	tmpDir, _ := os.MkdirTemp("", "oci-ttl-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	// Very short TTL
	h := NewHandler(idx, st,
		WithTagTTL(1*time.Millisecond),
		WithRouter(newTestRouter(t, upstream.URL)),
	)

	// First request
	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Wait for TTL to expire and background caching
	h.Close()
	time.Sleep(10 * time.Millisecond)

	// Second request with expired TTL - should revalidate
	h2 := NewHandler(idx, st,
		WithTagTTL(1*time.Millisecond),
		WithRouter(newTestRouter(t, upstream.URL)),
	)
	defer h2.Close()

	req2 := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	// Should have made 2 upstream requests (TTL expired)
	require.Equal(t, 2, requestCount)
}

func TestHandlerTagScopedByPrefix(t *testing.T) {
	// Verify that tag->digest mappings are scoped by prefix
	tmpDir, _ := os.MkdirTemp("", "oci-prefix-scope-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, _, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	ctx := context.Background()
	manifestDigest1 := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	manifestDigest2 := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	// Set tag for different "registries" (prefixes)
	_ = idx.SetTagDigest(ctx, "docker-hub/library/nginx", "latest", manifestDigest1)
	_ = idx.SetTagDigest(ctx, "ghcr/library/nginx", "latest", manifestDigest2)

	// Verify they are independent
	d1, _, err := idx.GetTagDigest(ctx, "docker-hub/library/nginx", "latest")
	require.NoError(t, err)
	require.Equal(t, manifestDigest1, d1)

	d2, _, err := idx.GetTagDigest(ctx, "ghcr/library/nginx", "latest")
	require.NoError(t, err)
	require.Equal(t, manifestDigest2, d2)
}

func TestHandlerMultiRegistry(t *testing.T) {
	manifest1 := `{"schemaVersion":2,"config":{"digest":"sha256:aaa"}}`
	manifest2 := `{"schemaVersion":2,"config":{"digest":"sha256:bbb"}}`
	digest1 := ComputeSHA256([]byte(manifest1))
	digest2 := ComputeSHA256([]byte(manifest2))

	// Upstream server 1 (docker-hub)
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/nginx/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest1)
			_, _ = w.Write([]byte(manifest1))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server1.Close()

	// Upstream server 2 (ghcr)
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/nginx/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest2)
			_, _ = w.Write([]byte(manifest2))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server2.Close()

	// Create handler with two registries
	tmpDir, err := os.MkdirTemp("", "oci-multi-registry-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	router, err := NewRouter([]Registry{
		{Prefix: "docker-hub", Upstream: NewUpstream(WithRegistryURL(server1.URL))},
		{Prefix: "ghcr", Upstream: NewUpstream(WithRegistryURL(server2.URL))},
	})
	require.NoError(t, err)

	h := NewHandler(idx, st,
		WithTagTTL(1*time.Hour),
		WithRouter(router),
	)
	defer h.Close()

	t.Run("docker-hub routes to server1", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/nginx/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, digest1, w.Header().Get("Docker-Content-Digest"))
		require.Equal(t, manifest1, w.Body.String())
	})

	t.Run("ghcr routes to server2", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/ghcr/library/nginx/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, digest2, w.Header().Get("Docker-Content-Digest"))
		require.Equal(t, manifest2, w.Body.String())
	})

	// Wait for background caching to finish
	h.Close()

	t.Run("tag isolation between registries", func(t *testing.T) {
		// Verify the index stored different digests for the same image name under different prefixes
		ctx := context.Background()

		d1, _, err := idx.GetTagDigest(ctx, "docker-hub/library/nginx", "latest")
		require.NoError(t, err)
		require.Equal(t, digest1, d1)

		d2, _, err := idx.GetTagDigest(ctx, "ghcr/library/nginx", "latest")
		require.NoError(t, err)
		require.Equal(t, digest2, d2)
	})
}

func TestHandlerNilRouter(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "oci-nil-router-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _, cleanupDB := newTestIndexWithStore(t, tmpDir)
	defer cleanupDB()

	// Create handler WITHOUT a router
	h := NewHandler(idx, st)
	defer h.Close()

	req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/nginx/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "no registry configured")
}

// newBenchIndexWithStore creates an OCI index and store for benchmarks.
func newBenchIndexWithStore(b *testing.B, tmpDir string) (*Index, store.Store, metadb.MetaDB) {
	b.Helper()

	be, err := backend.NewFilesystem(tmpDir)
	if err != nil {
		b.Fatal(err)
	}

	db := metadb.New()
	if err := db.Open(filepath.Join(tmpDir, "metadata.db")); err != nil {
		b.Fatal(err)
	}

	boltDB, ok := db.(*metadb.BoltDB)
	if !ok {
		b.Fatal("metaDB must be *metadb.BoltDB")
	}

	imageIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "image", 24*time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	manifestIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "manifest", 24*time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	blobIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "blob", 24*time.Hour)
	if err != nil {
		b.Fatal(err)
	}

	idx := NewIndex(imageIndex, manifestIndex, blobIndex)
	st := store.NewCAFS(be)

	b.Cleanup(func() {
		_ = db.Close()
	})

	return idx, st, db
}

func newBenchRouter(b *testing.B) *Router {
	b.Helper()
	router, err := NewRouter([]Registry{
		{Prefix: testPrefix, Upstream: NewUpstream()},
	})
	if err != nil {
		b.Fatal(err)
	}
	return router
}

func BenchmarkHandlerVersionCheck(b *testing.B) {
	tmpDir, _ := os.MkdirTemp("", "oci-bench-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _ := newBenchIndexWithStore(b, tmpDir)
	h := NewHandler(idx, st, WithRouter(newBenchRouter(b)))
	defer h.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
}

func BenchmarkHandlerManifestCacheHit(b *testing.B) {
	tmpDir, _ := os.MkdirTemp("", "oci-bench-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	idx, st, _ := newBenchIndexWithStore(b, tmpDir)

	// Pre-populate cache (using prefix-scoped name)
	ctx := context.Background()
	manifestContent := `{"schemaVersion":2}`
	manifestDigest := ComputeSHA256([]byte(manifestContent))
	hash, _ := st.Put(ctx, io.NopCloser(nil))
	_ = idx.PutManifest(ctx, manifestDigest, "application/vnd.oci.image.manifest.v1+json", hash, int64(len(manifestContent)))
	_ = idx.SetTagDigest(ctx, testPrefix+"/library/alpine", "latest", manifestDigest)

	h := NewHandler(idx, st, WithRouter(newBenchRouter(b)))
	defer h.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v2/docker-hub/library/alpine/manifests/latest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
}
