package git

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/download"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

const (
	fakeInfoRefsBody   = "001e# service=git-upload-pack\n0000"
	fakeUploadPackBody = "PACK\x00\x00\x00\x02\x00\x00\x00\x00" // fake pack data
)

// fakeGitUpstream creates a test server that mimics a Git smart HTTP server.
func fakeGitUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	var fetchCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", ContentTypeUploadPackAdvertisement)
			_, _ = w.Write([]byte(fakeInfoRefsBody))

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/git-upload-pack"):
			fetchCount.Add(1)
			w.Header().Set("Content-Type", ContentTypeUploadPackResult)
			_, _ = w.Write([]byte(fakeUploadPackBody))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(func() {
		srv.Close()
	})

	return srv
}

func TestParseGitPath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantHost   string
		wantRepo   string
		wantAction string
		wantErr    bool
	}{
		{
			name:       "simple repo",
			path:       "/github.com/user/repo.git/info/refs",
			wantHost:   "github.com",
			wantRepo:   "user/repo",
			wantAction: "info/refs",
		},
		{
			name:       "multi-segment GitLab path",
			path:       "/gitlab.com/group/sub/repo.git/git-upload-pack",
			wantHost:   "gitlab.com",
			wantRepo:   "group/sub/repo",
			wantAction: "git-upload-pack",
		},
		{
			name:       "git-receive-pack action",
			path:       "/github.com/user/repo.git/git-receive-pack",
			wantHost:   "github.com",
			wantRepo:   "user/repo",
			wantAction: "git-receive-pack",
		},
		{
			name:       "simple repo",
			path:       "/github.com/user/repo/info/refs",
			wantHost:   "github.com",
			wantRepo:   "user/repo",
			wantAction: "info/refs",
		},
		{
			name:       "multi-segment GitLab path",
			path:       "/gitlab.com/group/sub/repo/git-upload-pack",
			wantHost:   "gitlab.com",
			wantRepo:   "group/sub/repo",
			wantAction: "git-upload-pack",
		},
		{
			name:       "git-receive-pack action",
			path:       "/github.com/user/repo/git-receive-pack",
			wantHost:   "github.com",
			wantRepo:   "user/repo",
			wantAction: "git-receive-pack",
		},
		{
			name:    "path traversal",
			path:    "/github.com/../etc/passwd.git/info/refs",
			wantErr: true,
		},
		{
			name:    "empty segments",
			path:    "/github.com//repo.git/info/refs",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "/",
			wantErr: true,
		},
		{
			name:    "unknown action",
			path:    "/github.com/user/repo.git/unknown",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseGitPath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantHost, result.Repo.Host)
			require.Equal(t, tt.wantRepo, result.Repo.RepoPath)
			require.Equal(t, tt.wantAction, result.Action)
		})
	}
}

func TestHandlerInfoRefs(t *testing.T) {
	upstream := fakeGitUpstream(t)

	// Create handler that points upstream at the test server.
	// Since our upstream uses the repo's UpstreamURL (https://github.com/...),
	// we need to override the HTTP client's transport to redirect to our test server.
	h, cleanup := newTestHandlerWithTransport(t, upstream)
	defer cleanup()

	t.Run("passthrough", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/github.com/user/repo.git/info/refs?service=git-upload-pack", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, ContentTypeUploadPackAdvertisement, w.Header().Get("Content-Type"))
		require.Equal(t, fakeInfoRefsBody, w.Body.String())
	})

	t.Run("forwards Git-Protocol header", func(t *testing.T) {
		var receivedProtocol string
		protocolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedProtocol = r.Header.Get("Git-Protocol")
			w.Header().Set("Content-Type", ContentTypeUploadPackAdvertisement)
			_, _ = w.Write([]byte(fakeInfoRefsBody))
		}))
		defer protocolSrv.Close()

		ph, pcleanup := newTestHandlerWithTransport(t, protocolSrv)
		defer pcleanup()

		req := httptest.NewRequest(http.MethodGet, "/github.com/user/repo.git/info/refs?service=git-upload-pack", nil)
		req.Header.Set("Git-Protocol", "version=2")
		w := httptest.NewRecorder()

		ph.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "version=2", receivedProtocol)
	})

	t.Run("rejects git-receive-pack service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/github.com/user/repo.git/info/refs?service=git-receive-pack", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestHandlerUploadPack(t *testing.T) {
	upstream := fakeGitUpstream(t)
	h, cleanup := newTestHandlerWithTransport(t, upstream)
	defer cleanup()

	requestBody := []byte("0032want abc123\n00000009done\n")

	t.Run("cache miss then hit", func(t *testing.T) {
		// First request — cache miss, fetches from upstream
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(requestBody))
		req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, ContentTypeUploadPackResult, w.Header().Get("Content-Type"))
		firstBody := w.Body.String()

		// Second request — same body, should be cache hit
		req2 := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(requestBody))
		req2.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		w2 := httptest.NewRecorder()

		h.ServeHTTP(w2, req2)

		require.Equal(t, http.StatusOK, w2.Code)
		require.Equal(t, firstBody, w2.Body.String())
	})

	t.Run("cross-repo isolation", func(t *testing.T) {
		// Same request body to a different repo should not collide
		req := httptest.NewRequest(http.MethodPost, "/github.com/other/repo.git/git-upload-pack", bytes.NewReader(requestBody))
		req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		// Should succeed (fetches from upstream, not from cache of user/repo)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHandlerHostAllowlist(t *testing.T) {
	upstream := fakeGitUpstream(t)
	h, cleanup := newTestHandlerWithTransport(t, upstream)
	defer cleanup()

	t.Run("allowed host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/github.com/user/repo.git/info/refs?service=git-upload-pack", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("disallowed host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/evil.com/user/repo.git/info/refs?service=git-upload-pack", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestHandlerPushRejection(t *testing.T) {
	upstream := fakeGitUpstream(t)
	h, cleanup := newTestHandlerWithTransport(t, upstream)
	defer cleanup()

	t.Run("POST git-receive-pack is forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-receive-pack", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestHandlerRequestBodyLimit(t *testing.T) {
	upstream := fakeGitUpstream(t)

	tmpDir, err := os.MkdirTemp("", "git-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	defer func() { _ = db.Close() }()

	packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(packIdx)
	dl := download.New()

	h := NewHandler(idx, cafs,
		WithUpstream(NewUpstream(WithHTTPClient(redirectClient(upstream.URL)))),
		WithDownloader(dl),
		WithAllowedHosts([]string{"github.com"}),
		WithMaxRequestBodySize(10), // 10 bytes max
	)

	// Send body larger than limit
	bigBody := bytes.Repeat([]byte("x"), 100)
	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(bigBody))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestHandlerSingleflight(t *testing.T) {
	var fetchCount atomic.Int32
	var fetchGate sync.WaitGroup
	fetchGate.Add(1)

	slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/git-upload-pack") {
			fetchCount.Add(1)
			fetchGate.Wait() // Block until released
			w.Header().Set("Content-Type", ContentTypeUploadPackResult)
			_, _ = w.Write([]byte(fakeUploadPackBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer slowUpstream.Close()

	h, cleanup := newTestHandlerWithTransport(t, slowUpstream)
	defer cleanup()

	body := []byte("0032want abc123\n00000009done\n")

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 3)

	for i := range 3 {
		wg.Add(1)
		results[i] = httptest.NewRecorder()
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
			h.ServeHTTP(results[idx], req)
		}(i)
	}

	// Allow some time for all requests to hit the downloader
	time.Sleep(50 * time.Millisecond)

	// Release the upstream
	fetchGate.Done()
	wg.Wait()

	// Only one upstream fetch should have occurred
	require.Equal(t, int32(1), fetchCount.Load(), "expected single upstream fetch due to singleflight")

	// All responses should succeed
	for i, w := range results {
		require.Equal(t, http.StatusOK, w.Code, "request %d failed", i)
	}
}

func TestHandlerUploadPackGzip(t *testing.T) {
	upstream := fakeGitUpstream(t)
	h, cleanup := newTestHandlerWithTransport(t, upstream)
	defer cleanup()

	requestBody := []byte("0032want abc123\n00000009done\n")

	// Compress the request body
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err := gz.Write(requestBody)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	t.Run("gzip request body is decompressed and cached", func(t *testing.T) {
		// Send gzip-encoded request
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/gzip-repo.git/git-upload-pack", bytes.NewReader(gzBuf.Bytes()))
		req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, ContentTypeUploadPackResult, w.Header().Get("Content-Type"))
		firstBody := w.Body.String()

		// Send the same body uncompressed — should be a cache hit
		// because the decompressed body produces the same hash
		req2 := httptest.NewRequest(http.MethodPost, "/github.com/user/gzip-repo.git/git-upload-pack", bytes.NewReader(requestBody))
		req2.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		w2 := httptest.NewRecorder()

		h.ServeHTTP(w2, req2)

		require.Equal(t, http.StatusOK, w2.Code)
		require.Equal(t, firstBody, w2.Body.String())
	})

	t.Run("invalid gzip body returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/gzip-repo.git/git-upload-pack", bytes.NewReader([]byte("not gzip")))
		req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestHandlerDecompressedBodyLimit(t *testing.T) {
	upstream := fakeGitUpstream(t)

	tmpDir, err := os.MkdirTemp("", "git-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	defer func() { _ = db.Close() }()

	packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(packIdx)
	dl := download.New()

	// Set the decompressed limit to a small value so we can trigger it in a test.
	h := NewHandler(idx, cafs,
		WithUpstream(NewUpstream(WithHTTPClient(redirectClient(upstream.URL)))),
		WithDownloader(dl),
		WithAllowedHosts([]string{"github.com"}),
		WithMaxDecompressedBodySize(10), // 10 bytes decompressed limit
	)

	// Build a gzip body whose decompressed form exceeds the 10-byte limit.
	plainBody := bytes.Repeat([]byte("x"), 100)
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err = gz.Write(plainBody)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(gzBuf.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestHandlerUploadPackCacheHitSkipsUpstream(t *testing.T) {
	var fetchCount atomic.Int32

	countingUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", ContentTypeUploadPackAdvertisement)
			_, _ = w.Write([]byte(fakeInfoRefsBody))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/git-upload-pack"):
			fetchCount.Add(1)
			w.Header().Set("Content-Type", ContentTypeUploadPackResult)
			_, _ = w.Write([]byte(fakeUploadPackBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer countingUpstream.Close()

	h, cleanup := newTestHandlerWithTransport(t, countingUpstream)
	defer cleanup()

	body := []byte("0032want def456\n00000009done\n")

	// First request — cache miss
	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int32(1), fetchCount.Load())

	// Second request — cache hit, upstream must not be called again
	req2 := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, int32(1), fetchCount.Load(), "cache hit should not call upstream")
}

func TestHandlerUploadPackCacheHitRequiresCurrentAuth(t *testing.T) {
	var fetchCount atomic.Int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/git-upload-pack") {
			fetchCount.Add(1)
			w.Header().Set("Content-Type", ContentTypeUploadPackResult)
			_, _ = w.Write([]byte(fakeUploadPackBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstreamSrv.Close()

	h, cleanup := newTestHandlerWithTransport(t, upstreamSrv)
	defer cleanup()

	authProvider := &toggleBasicAuthProvider{}
	authProvider.allow.Store(true)
	upstream := NewUpstream(
		WithHTTPClient(redirectClient(upstreamSrv.URL)),
		WithBasicAuthProvider(authProvider),
	)
	router, err := NewRouter(nil, WithFallback(upstream))
	require.NoError(t, err)
	h.router = router

	body := []byte("0032want cached-auth\n00000009done\n")

	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int32(1), fetchCount.Load())

	authProvider.allow.Store(false)
	req2 := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusBadGateway, w2.Code)
	require.Equal(t, int32(1), fetchCount.Load(), "auth failure on cache hit must not refetch upstream")
	require.NotContains(t, w2.Body.String(), fakeUploadPackBody)
}

func TestHandlerUpstreamErrors(t *testing.T) {
	t.Run("upstream 404 returns 404", func(t *testing.T) {
		notFoundUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer notFoundUpstream.Close()

		h, cleanup := newTestHandlerWithTransport(t, notFoundUpstream)
		defer cleanup()

		req := httptest.NewRequest(http.MethodGet, "/github.com/user/nonexistent.git/info/refs?service=git-upload-pack", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("upstream 500 returns 502", func(t *testing.T) {
		errorUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer errorUpstream.Close()

		h, cleanup := newTestHandlerWithTransport(t, errorUpstream)
		defer cleanup()

		req := httptest.NewRequest(http.MethodGet, "/github.com/user/broken.git/info/refs?service=git-upload-pack", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadGateway, w.Code)
	})

	t.Run("upload-pack upstream 404 returns 404", func(t *testing.T) {
		notFoundUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer notFoundUpstream.Close()

		h, cleanup := newTestHandlerWithTransport(t, notFoundUpstream)
		defer cleanup()

		body := []byte("0032want abc123\n00000009done\n")
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/nonexistent.git/git-upload-pack", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerStaleCacheEviction(t *testing.T) {
	upstream := fakeGitUpstream(t)

	tmpDir, err := os.MkdirTemp("", "git-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	defer func() { _ = db.Close() }()

	packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(packIdx)
	dl := download.New()

	h := NewHandler(idx, cafs,
		WithUpstream(NewUpstream(WithHTTPClient(redirectClient(upstream.URL)))),
		WithDownloader(dl),
		WithAllowedHosts([]string{"github.com"}),
	)

	body := []byte("0032want stale1\n00000009done\n")

	// First request — populates cache
	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	firstBody := w.Body.String()

	// Get the cached entry to find the stored hash
	ctx := context.Background()
	bodyHash := contentcache.HashBytes(body)
	cacheKey := fmt.Sprintf("github.com/user/repo::%s", bodyHash.String())
	cached, err := idx.GetCachedPack(ctx, cacheKey)
	require.NoError(t, err)

	// Delete the blob from CAFS to simulate stale cache
	err = cafs.Delete(ctx, cached.ResponseHash)
	require.NoError(t, err)

	// Next request should detect stale entry, evict it, and re-fetch from upstream
	req2 := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, firstBody, w2.Body.String())
}

// pktLine encodes s as a git pkt-line: a 4-hex-digit length prefix covering the
// prefix itself plus the payload, followed by the payload.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

const (
	flushPkt = "0000"
	delimPkt = "0001"
)

func TestIsLsRefs(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{
			name: "ls-refs command only",
			body: pktLine("command=ls-refs\n"),
			want: true,
		},
		{
			name: "ls-refs with capabilities, delimiter and args",
			body: pktLine("command=ls-refs\n") +
				pktLine("object-format=sha1\n") +
				delimPkt +
				pktLine("peel\n") +
				pktLine("ref-prefix HEAD\n") +
				flushPkt,
			want: true,
		},
		{
			name: "ls-refs preceded by control packets",
			body: flushPkt + delimPkt + pktLine("command=ls-refs\n"),
			want: true,
		},
		{
			name: "fetch command is not ls-refs",
			body: pktLine("command=fetch\n") +
				pktLine("object-format=sha1\n") +
				delimPkt +
				pktLine("want 0123456789012345678901234567890123456789\n") +
				flushPkt,
			want: false,
		},
		{
			name: "v1-style want/done body has no command",
			body: pktLine("want 0123456789012345678901234567890123456789\n") +
				flushPkt +
				pktLine("done\n"),
			want: false,
		},
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "only a flush packet",
			body: flushPkt,
			want: false,
		},
		{
			name:    "malformed packet length below minimum",
			body:    "0003",
			wantErr: true,
		},
		{
			name:    "invalid hex length",
			body:    "zzzzcommand=ls-refs\n",
			wantErr: true,
		},
		{
			name: "truncated packet is treated as not ls-refs",
			body: "0050command=ls-refs\n", // declares 0x50 bytes but the body is far shorter
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isLsRefs(strings.NewReader(tt.body))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsLsRefsResetsPosition(t *testing.T) {
	body := pktLine("command=ls-refs\n") + flushPkt
	r := strings.NewReader(body)

	got, err := isLsRefs(r)
	require.NoError(t, err)
	require.True(t, got)

	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, body, string(rest), "body must be readable from the start after isLsRefs")
}

func TestIsLsRefsSeeksToStartBeforeScanning(t *testing.T) {
	body := pktLine("command=ls-refs\n")
	r := strings.NewReader(body)

	_, err := r.Seek(4, io.SeekStart)
	require.NoError(t, err)

	got, err := isLsRefs(r)
	require.NoError(t, err)
	require.True(t, got)
}

// countingUploadPackUpstream returns a test server that records every
// git-upload-pack request body and responds with a fake pack.
func countingUploadPackUpstream(fetchCount *atomic.Int32, lastBody *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/git-upload-pack") {
			fetchCount.Add(1)
			if b, err := io.ReadAll(r.Body); err == nil {
				*lastBody = b
			}
			w.Header().Set("Content-Type", ContentTypeUploadPackResult)
			_, _ = w.Write([]byte(fakeUploadPackBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestHandlerUploadPackLsRefsBypassesCache(t *testing.T) {
	var fetchCount atomic.Int32
	var lastBody []byte
	upstreamSrv := countingUploadPackUpstream(&fetchCount, &lastBody)
	defer upstreamSrv.Close()

	h, cleanup := newTestHandlerWithTransport(t, upstreamSrv)
	defer cleanup()

	body := []byte(pktLine("command=ls-refs\n") +
		pktLine("object-format=sha1\n") +
		delimPkt +
		pktLine("peel\n") +
		pktLine("ref-prefix HEAD\n") +
		flushPkt)

	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(body))
		req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
		req.Header.Set("Git-Protocol", "version=2")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "request %d", i)
		require.Equal(t, ContentTypeUploadPackResult, w.Header().Get("Content-Type"))
		require.Equal(t, fakeUploadPackBody, w.Body.String())
	}

	require.Equal(t, int32(2), fetchCount.Load(), "ls-refs must reach upstream on every request and never be cached")
	require.Equal(t, body, lastBody, "the full ls-refs body must be forwarded to upstream")
}

func TestHandlerUploadPackLsRefsGzip(t *testing.T) {
	var fetchCount atomic.Int32
	var lastBody []byte
	upstreamSrv := countingUploadPackUpstream(&fetchCount, &lastBody)
	defer upstreamSrv.Close()

	h, cleanup := newTestHandlerWithTransport(t, upstreamSrv)
	defer cleanup()

	plain := []byte(pktLine("command=ls-refs\n") + flushPkt)

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err := gz.Write(plain)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	req := httptest.NewRequest(http.MethodPost, "/github.com/user/repo.git/git-upload-pack", bytes.NewReader(gzBuf.Bytes()))
	req.Header.Set("Content-Type", ContentTypeUploadPackRequest)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Git-Protocol", "version=2")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, fakeUploadPackBody, w.Body.String())
	require.Equal(t, int32(1), fetchCount.Load())
	require.Equal(t, plain, lastBody, "the decompressed ls-refs body must be forwarded to upstream")
}

// newTestHandlerWithTransport creates a test handler that redirects all HTTPS
// requests to the given test server via a custom transport.
func newTestHandlerWithTransport(t *testing.T, targetServer *httptest.Server) (*Handler, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "git-test-*")
	require.NoError(t, err)

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))

	packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(packIdx)
	dl := download.New()

	upstream := NewUpstream(WithHTTPClient(redirectClient(targetServer.URL)))

	h := NewHandler(idx, cafs,
		WithUpstream(upstream),
		WithDownloader(dl),
		WithAllowedHosts([]string{"github.com", "gitlab.com"}),
	)

	return h, func() {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

// redirectClient creates an HTTP client that rewrites all request URLs
// to point at the given target URL, preserving the path and query.
func redirectClient(targetURL string) *http.Client {
	return &http.Client{
		Transport: &rewriteTransport{target: targetURL},
	}
}

type toggleBasicAuthProvider struct {
	allow atomic.Bool
}

func (p *toggleBasicAuthProvider) BasicAuth(context.Context, RepoRef) (string, string, error) {
	if !p.allow.Load() {
		return "", "", fmt.Errorf("auth unavailable")
	}
	return "x-access-token", "token", nil
}

type rewriteTransport struct {
	target string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the URL to point at the test server
	newURL := rt.target + req.URL.Path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body) //nolint:gosec // newURL is constructed from test server address + request path, not user input
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return http.DefaultTransport.RoundTrip(newReq)
}
