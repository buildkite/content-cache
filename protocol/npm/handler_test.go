package npm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

// setupMetaDB creates a BoltDB with EnvelopeIndex instances for testing.
// Returns the database, index, and a cleanup function that closes the database.
func setupMetaDB(t *testing.T, tmpDir string) (*metadb.BoltDB, *Index, func()) {
	t.Helper()
	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	metadataIdx, err := metadb.NewEnvelopeIndex(db, "npm", "metadata", 24*time.Hour)
	require.NoError(t, err)
	cacheIdx, err := metadb.NewEnvelopeIndex(db, "npm", "cache", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(metadataIdx, cacheIdx)
	return db, idx, func() { _ = db.Close() }
}

func newTestHandler(t *testing.T, upstreamServer *httptest.Server) (*Handler, func()) {
	t.Helper()
	// Use a manual temp dir instead of t.TempDir() to avoid race with async goroutines
	tmpDir, err := os.MkdirTemp("", "npm-test-*")
	require.NoError(t, err)
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)

	opts := []UpstreamOption{}
	if upstreamServer != nil {
		opts = append(opts, WithRegistryURL(upstreamServer.URL))
	}
	upstream := NewUpstream(opts...)

	h := NewHandler(idx, cafs, WithUpstream(upstream))
	return h, func() {
		// Wait for background goroutines to complete before cleanup
		h.Close()
		closeDB()
		_ = os.RemoveAll(tmpDir)
	}
}

func TestHandlerMetadata(t *testing.T) {
	// Create mock upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lodash" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "lodash",
				"dist-tags": {"latest": "4.17.21"},
				"versions": {
					"4.17.21": {
						"name": "lodash",
						"version": "4.17.21",
						"dist": {
							"tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
							"shasum": "abc123"
						}
					}
				}
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("get metadata", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/lodash", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), `"name":"lodash"`)
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/nonexistent-pkg", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("abbreviated metadata", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/lodash", nil)
		req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		// Abbreviated response should have limited fields
		body := w.Body.String()
		require.Contains(t, body, `"name"`)
	})
}

func TestHandlerMetadataCacheNotAbbreviated(t *testing.T) {
	// Regression test: if the first request is abbreviated, the cache must still
	// store the full metadata so that a subsequent full request gets all fields.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name": "mypkg",
			"description": "a full description",
			"dist-tags": {"latest": "1.0.0"},
			"versions": {
				"1.0.0": {
					"name": "mypkg",
					"version": "1.0.0",
					"dist": {
						"tarball": "https://registry.npmjs.org/mypkg/-/mypkg-1.0.0.tgz",
						"shasum": "abc"
					}
				}
			}
		}`))
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	// First request: abbreviated (cache miss — populates cache).
	req1 := httptest.NewRequest(http.MethodGet, "/mypkg", nil)
	req1.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusOK, w1.Code)

	// Wait briefly for the async cache write to complete.
	h.Close()

	// Recreate handler reusing the same index/store so the cache is warm.
	// (newTestHandler creates fresh storage; instead re-serve through the same h.)
	// Re-open by creating a second handler backed by the same upstream (which now
	// counts hits) — but simpler: just make a second request through the same handler.
	// The cache write goroutine is done after h.Close() above, so we need a new handler.
	// Use the upstream call count to distinguish cache miss vs hit.
	callCount := 0
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"mypkg","description":"a full description","versions":{}}`))
	}))
	defer upstream2.Close()

	h2, cleanup2 := newTestHandler(t, upstream2)
	defer cleanup2()

	// Warm the cache with an abbreviated request.
	reqA := httptest.NewRequest(http.MethodGet, "/mypkg", nil)
	reqA.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	wA := httptest.NewRecorder()
	h2.ServeHTTP(wA, reqA)
	require.Equal(t, http.StatusOK, wA.Code)
	require.Equal(t, 1, callCount, "first request should fetch from upstream")

	h2.wg.Wait() // wait for cache write

	// Second request: full metadata — must come from cache (callCount stays at 1)
	// and must include the "description" field that abbreviation strips.
	reqB := httptest.NewRequest(http.MethodGet, "/mypkg", nil)
	wB := httptest.NewRecorder()
	h2.ServeHTTP(wB, reqB)
	require.Equal(t, http.StatusOK, wB.Code)
	require.Equal(t, 1, callCount, "second request should be served from cache")
	require.Contains(t, wB.Body.String(), `"description"`, "full response must include description field")
}

func TestHandlerTarball(t *testing.T) {
	tarballContent := []byte("fake-tarball-content-12345")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test-pkg" {
			w.Header().Set("Content-Type", "application/json")
			body := fmt.Sprintf(`{
				"name": "test-pkg",
				"versions": {
					"1.0.0": {
						"name": "test-pkg",
						"version": "1.0.0",
						"dist": {
							"tarball": "%s/test-pkg/-/test-pkg-1.0.0.tgz",
							"shasum": "8a698aa23442e52ee3e7009f7f8578d8ee3c6bc1"
						}
					}
				}
			}`, r.Host)
			_, _ = w.Write([]byte(body)) //nolint:gosec // test server writing mock npm metadata response
			return
		}
		if r.URL.Path == "/test-pkg/-/test-pkg-1.0.0.tgz" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarballContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("download tarball", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test-pkg/-/test-pkg-1.0.0.tgz", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, string(tarballContent), w.Body.String())
	})

	t.Run("tarball not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/nonexistent/-/nonexistent-1.0.0.tgz", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerScopedPackage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// URL should have encoded scope
		if r.URL.Path == "/@babel%2fcore" || r.URL.Path == "/@babel/core" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "@babel/core",
				"versions": {
					"7.23.0": {
						"name": "@babel/core",
						"version": "7.23.0",
						"dist": {
							"tarball": "https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz",
							"shasum": "1b37b9c77f147faf4f645dfa32d7b233233b0755"
						}
					}
				}
			}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/-/core-7.23.0.tgz") {
			_, _ = w.Write([]byte("scoped-tarball-content"))
			return
		}
		t.Logf("404 for path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("scoped package metadata", func(t *testing.T) {
		// URL-encoded scoped package
		req := httptest.NewRequest(http.MethodGet, "/@babel%2fcore", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	})

	t.Run("scoped tarball", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/@babel%2fcore/-/core-7.23.0.tgz", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t, nil)
	defer cleanup()

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/test", nil)
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}

func TestHandlerTarballURLRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name": "test-pkg",
			"versions": {
				"1.0.0": {
					"name": "test-pkg",
					"version": "1.0.0",
					"dist": {
						"tarball": "https://registry.npmjs.org/test-pkg/-/test-pkg-1.0.0.tgz"
					}
				}
			}
		}`))
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/test-pkg", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	// URL should be rewritten to point to our proxy
	require.Contains(t, body, "http://localhost:8080/npm/test-pkg/-/test-pkg-1.0.0.tgz")
	require.NotContains(t, body, "registry.npmjs.org")
}

func TestExtractVersionFromTarball(t *testing.T) {
	tests := []struct {
		name        string
		packageName string
		tarballName string
		want        string
	}{
		{
			name:        "simple package",
			packageName: "lodash",
			tarballName: "lodash-4.17.21",
			want:        "4.17.21",
		},
		{
			name:        "scoped package",
			packageName: "@babel/core",
			tarballName: "core-7.23.0",
			want:        "7.23.0",
		},
		{
			name:        "prerelease version",
			packageName: "test",
			tarballName: "test-1.0.0-beta.1",
			want:        "1.0.0-beta.1",
		},
		{
			name:        "no match",
			packageName: "other",
			tarballName: "test-1.0.0",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVersionFromTarball(tt.packageName, tt.tarballName)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestHandlerCacheHit(t *testing.T) {
	tarballContent := []byte("cached-tarball-content")
	fetchCount := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cached-pkg" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "cached-pkg",
				"versions": {
					"1.0.0": {
						"name": "cached-pkg",
						"version": "1.0.0",
						"dist": {"tarball": "http://example.com/cached-pkg/-/cached-pkg-1.0.0.tgz"}
					}
				}
			}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "cached-pkg-1.0.0.tgz") {
			fetchCount++
			_, _ = w.Write(tarballContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	// First request - should fetch from upstream
	req1 := httptest.NewRequest(http.MethodGet, "/cached-pkg/-/cached-pkg-1.0.0.tgz", nil)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)

	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, string(tarballContent), w1.Body.String())
	require.Equal(t, 1, fetchCount)
}
