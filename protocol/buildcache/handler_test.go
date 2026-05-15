package buildcache

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

	entryIndex, err := metadb.NewEnvelopeIndex(db, "buildcache", "entry", 24*time.Hour)
	require.NoError(t, err)

	idx := NewIndex(entryIndex)
	cafsStore := store.NewCAFS(b)
	handler := NewHandler(idx, cafsStore)

	return handler, idx, cafsStore
}

func TestHandlerGetMiss(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandlerPutThenGet(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	blobContent := "hello build cache"
	actionID := "aa" + strings.Repeat("00", 31)
	outputID := "bb" + strings.Repeat("00", 31)

	// PUT the blob.
	putReq := httptest.NewRequest(http.MethodPut, "/"+actionID+"?output_id="+outputID, bytes.NewReader([]byte(blobContent)))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusNoContent, putRec.Code)

	// GET the blob.
	getReq := httptest.NewRequest(http.MethodGet, "/"+actionID, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)

	require.Equal(t, http.StatusOK, getRec.Code)
	require.Equal(t, outputID, getRec.Header().Get("X-Output-ID"))
	body, _ := io.ReadAll(getRec.Body)
	require.Equal(t, blobContent, string(body))
	require.Equal(t, "17", getRec.Header().Get("Content-Length"))
}

func TestHandlerMissingActionID(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "missing action ID")
}

func TestHandlerInvalidActionID(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	tests := []struct {
		name     string
		actionID string
	}{
		{"not hex", "/zzzzzzzz"},
		{"odd length", "/abc"},
		{"path traversal", "/..%2f..%2fetc%2fpasswd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.actionID, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/aa00", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandlerPutMissingOutputID(t *testing.T) {
	handler, _, _ := newTestHandler(t)

	actionID := "aa" + strings.Repeat("00", 31)
	req := httptest.NewRequest(http.MethodPut, "/"+actionID, bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "missing output_id")
}

func TestIndexGetPut(t *testing.T) {
	tmpDir := t.TempDir()
	db := metadb.NewBoltDB()
	err := db.Open(filepath.Join(tmpDir, "meta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	entryIndex, err := metadb.NewEnvelopeIndex(db, "buildcache", "entry", 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(entryIndex)

	ctx := context.Background()

	// Get non-existent.
	_, err = idx.Get(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)

	// Put and get.
	blobHash := "blake3:" + strings.Repeat("cc", 32)
	entry := &ActionEntry{
		OutputID: strings.Repeat("bb", 32),
		BlobHash: blobHash,
		Size:     42,
	}
	require.NoError(t, idx.Put(ctx, "aa00", entry))

	got, err := idx.Get(ctx, "aa00")
	require.NoError(t, err)
	require.Equal(t, entry.OutputID, got.OutputID)
	require.Equal(t, entry.BlobHash, got.BlobHash)
	require.Equal(t, entry.Size, got.Size)
}

func TestIsValidHex(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"aa", true},
		{"aabb", true},
		{"AABB", true},
		{"", false},
		{"a", false},   // odd length
		{"zz", false},  // not hex
		{"abc", false}, // odd length
		{"0123456789abcdef", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.want, isValidHex(tt.input))
		})
	}
}
