package goproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSumdbHandler_Supported(t *testing.T) {
	h := newTestSumdbHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/supported", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestSumdbHandler_Supported_InvalidName(t *testing.T) {
	h := newTestSumdbHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/sumdb/evil.example.com/supported", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSumdbHandler_Latest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/latest", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("go.sum database tree\n123\nhash\nsig\n"))
	}))
	defer upstream.Close()

	h := newTestSumdbHandler(t, upstream)

	req := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/latest", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "go.sum database tree")
}

func TestSumdbHandler_Lookup_Cached(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		assert.Equal(t, "/lookup/github.com/example/pkg@v1.0.0", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("123\ngithub.com/example/pkg v1.0.0 h1:abc123=\n"))
	}))
	defer upstream.Close()

	h := newTestSumdbHandler(t, upstream)

	// First request - cache miss
	req1 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/lookup/github.com/example/pkg@v1.0.0", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, int32(1), callCount.Load())

	// Second request - cache hit
	req2 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/lookup/github.com/example/pkg@v1.0.0", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, int32(1), callCount.Load()) // Still 1, served from cache
	require.Equal(t, rec1.Body.String(), rec2.Body.String())
}

func TestSumdbHandler_Tile_Cached(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		assert.Equal(t, "/tile/8/0/001", r.URL.Path)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("tile-data-here"))
	}))
	defer upstream.Close()

	h := newTestSumdbHandler(t, upstream)

	// First request - cache miss
	req1 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/tile/8/0/001", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, int32(1), callCount.Load())

	// Second request - cache hit
	req2 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/tile/8/0/001", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, int32(1), callCount.Load()) // Still 1, served from cache
}

func TestSumdbHandler_Tile_NotFound_NotCached(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := newTestSumdbHandler(t, upstream)

	// First request - 404
	req1 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/tile/8/0/999", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	require.Equal(t, http.StatusNotFound, rec1.Code)
	require.Equal(t, int32(1), callCount.Load())

	// Second request - should still hit upstream (404 not cached)
	req2 := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/tile/8/0/999", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusNotFound, rec2.Code)
	require.Equal(t, int32(2), callCount.Load()) // Called again, 404 not cached
}

func TestSumdbHandler_MethodNotAllowed(t *testing.T) {
	h := newTestSumdbHandler(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/sumdb/sum.golang.org/supported", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestSumdbHandler_InvalidEndpoint(t *testing.T) {
	h := newTestSumdbHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/invalid", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// newTestSumdbHandler creates a SumdbHandler for testing.
// If upstream is nil, a dummy upstream is created.
func newTestSumdbHandler(t *testing.T, upstream *httptest.Server) *SumdbHandler {
	t.Helper()

	// Create in-memory index
	boltDB := metadb.NewBoltDB(metadb.WithNoSync(true))
	tmpFile := t.TempDir() + "/test.db"
	err := boltDB.Open(tmpFile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = boltDB.Close() })

	envelope, err := metadb.NewEnvelopeIndex(boltDB, "sumdb", "cache", 0)
	require.NoError(t, err)

	index := NewSumdbIndex(envelope)

	opts := []SumdbHandlerOption{}

	if upstream != nil {
		opts = append(opts, WithSumdbUpstream(NewSumdbUpstream(
			WithSumdbUpstreamURL(upstream.URL),
		)))
	}

	return NewSumdbHandler(index, nil, opts...)
}
