package httpcache

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func newTestHandler(t *testing.T) (*Handler, *Index, store.Store) {
	t.Helper()

	tmpDir := t.TempDir()
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)

	db := metadb.NewBoltDB()
	dbPath := filepath.Join(tmpDir, "metadata.db")
	err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	entryIndex, err := metadb.NewEnvelopeIndex(db, "httpcache", "entry", 24*time.Hour)
	require.NoError(t, err)

	idx := NewIndex(entryIndex)
	cafsStore := store.NewCAFS(b)
	handler := NewHandler(idx, cafsStore)

	return handler, idx, cafsStore
}

func TestHandlerGetMiss(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/abc123", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandlerPutThenGet(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	blobContent := "hello sccache"
	key := "abc123def456"

	// PUT the blob.
	putReq := httptest.NewRequest(http.MethodPut, "/"+key, bytes.NewReader([]byte(blobContent)))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusNoContent, putRec.Code)

	// GET the blob.
	getReq := httptest.NewRequest(http.MethodGet, "/"+key, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)

	require.Equal(t, http.StatusOK, getRec.Code)
	body, _ := io.ReadAll(getRec.Body)
	require.Equal(t, blobContent, string(body))
	require.Equal(t, "13", getRec.Header().Get("Content-Length"))
	require.Empty(t, getRec.Header().Get("X-Output-ID")) // not part of this protocol
}

func TestHandlerMissingKey(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandlerInvalidKey(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	tests := []struct {
		name string
		path string
	}{
		{"empty", "/"},
		{"path traversal", "/../etc/passwd"},
		{"path traversal segment", "/a/../b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestHandlerMKCOL(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	// sccache sends MKCOL to create directory prefixes before PUT.
	req := httptest.NewRequest("MKCOL", "/a/b/c/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestHandlerPROPFIND(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest("PROPFIND", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, 207, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/xml")
	require.Contains(t, rec.Body.String(), "multistatus")
	require.Contains(t, rec.Body.String(), "getlastmodified") // required by opendal/sccache
}

func TestHandlerSlashKey(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	// sccache uses a/b/c/{hash} key structure.
	key := "a/b/c/abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	content := []byte("sccache artifact")

	putReq := httptest.NewRequest(http.MethodPut, "/"+key, bytes.NewReader(content))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusNoContent, putRec.Code)

	getReq := httptest.NewRequest(http.MethodGet, "/"+key, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	body, _ := io.ReadAll(getRec.Body)
	require.Equal(t, content, body)
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/somekey", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandlerGradleStyleKey(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	// Gradle uses lowercase hex MD5/SHA hashes as keys.
	key := "5e2f5d0e74f7c27b2c2dc3d62f7fc940"
	content := []byte("gradle build artifact")

	putReq := httptest.NewRequest(http.MethodPut, "/"+key, bytes.NewReader(content))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusNoContent, putRec.Code)

	getReq := httptest.NewRequest(http.MethodGet, "/"+key, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	body, _ := io.ReadAll(getRec.Body)
	require.Equal(t, content, body)
}

func TestIndexGetPut(t *testing.T) {
	tmpDir := t.TempDir()
	db := metadb.NewBoltDB()
	err := db.Open(filepath.Join(tmpDir, "meta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	entryIndex, err := metadb.NewEnvelopeIndex(db, "httpcache", "entry", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(entryIndex)

	ctx := context.Background()

	// Get non-existent.
	_, err = idx.Get(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)

	// Put and get.
	blobHash := "blake3:" + strings.Repeat("cc", 32)
	entry := &CacheEntry{
		BlobHash: blobHash,
		Size:     42,
	}
	require.NoError(t, idx.Put(ctx, "mykey", entry))

	got, err := idx.Get(ctx, "mykey")
	require.NoError(t, err)
	require.Equal(t, entry.BlobHash, got.BlobHash)
	require.Equal(t, entry.Size, got.Size)
}

func TestHandlerPutReplacesExistingKey(t *testing.T) {
	handler, _, _ := newTestHandler(t)
	key := "replacekey"

	for i, content := range []string{"first content", "second content"} {
		putReq := httptest.NewRequest(http.MethodPut, "/"+key, bytes.NewReader([]byte(content)))
		putRec := httptest.NewRecorder()
		handler.ServeHTTP(putRec, putReq)
		require.Equal(t, http.StatusNoContent, putRec.Code, "PUT %d failed", i)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/"+key, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	body, _ := io.ReadAll(getRec.Body)
	require.Equal(t, "second content", string(body))
}

func TestHandlerIndexHitBlobMissing(t *testing.T) {
	handler, idx, _ := newTestHandler(t)

	// Inject an index entry pointing to a non-existent blob.
	err := idx.Put(context.Background(), "ghostkey", &CacheEntry{
		BlobHash: "blake3:" + strings.Repeat("aa", 32),
		Size:     42,
	})
	require.NoError(t, err)

	getReq := httptest.NewRequest(http.MethodGet, "/ghostkey", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusNotFound, getRec.Code)
}

func TestHandlerBodyTooLarge(t *testing.T) {
	handler, _, _ := newTestHandler(t)
	handler.maxBodySize = 10 // override for test

	body := bytes.NewReader(bytes.Repeat([]byte("x"), 11))
	putReq := httptest.NewRequest(http.MethodPut, "/bigkey", body)
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusRequestEntityTooLarge, putRec.Code)
}

func TestHandlerHeadNotAllowed(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodHead, "/somekey", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestIsValidKey(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"abc", true},
		{"5e2f5d0e74f7c27b2c2dc3d62f7fc940", true},
		{"key-with-dashes", true},
		{"key_with_underscores", true},
		{"a/b/c/hash", true},              // sccache sharding style
		{"b/c/a/bcacdff48b6c1753c", true}, // sccache sharding style
		{"", false},                       // empty
		{"a/../b", false},                 // path traversal
		{"../../etc/passwd", false},       // path traversal
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.want, isValidKey(tt.input))
		})
	}
}
