package goproxy

import (
	"context"
	"encoding/json"
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

func TestHandlerList(t *testing.T) {
	// Setup mock upstream
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/@v/list") {
			_, _ = w.Write([]byte("v1.0.0\nv1.1.0\nv1.2.0\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	// Request version list
	req := httptest.NewRequest(http.MethodGet, "/github.com/test/module/@v/list", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "v1.0.0")
}

func TestHandlerInfo(t *testing.T) {
	infoTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.info") {
			_ = json.NewEncoder(w).Encode(VersionInfo{
				Version: "v1.0.0",
				Time:    infoTime,
			})
			return
		}
		// Also handle mod and zip for background caching
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.mod") {
			_, _ = w.Write([]byte("module github.com/test/module\n"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.zip") {
			_, _ = w.Write([]byte("fake zip content"))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/github.com/test/module/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var info VersionInfo
	err := json.NewDecoder(rec.Body).Decode(&info)
	require.NoError(t, err)
	require.Equal(t, "v1.0.0", info.Version)
}

func TestHandlerMod(t *testing.T) {
	modContent := "module github.com/test/module\n\ngo 1.21\n"

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.mod") {
			_, _ = w.Write([]byte(modContent))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.info") {
			_ = json.NewEncoder(w).Encode(VersionInfo{Version: "v1.0.0"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.zip") {
			_, _ = w.Write([]byte("fake zip"))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/github.com/test/module/@v/v1.0.0.mod", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, modContent, rec.Body.String())
}

func TestHandlerZip(t *testing.T) {
	zipContent := "PK\x03\x04fake zip content"

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.zip") {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write([]byte(zipContent))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.info") {
			_ = json.NewEncoder(w).Encode(VersionInfo{Version: "v1.0.0"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.mod") {
			_, _ = w.Write([]byte("module test\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/github.com/test/module/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	require.Equal(t, zipContent, rec.Body.String())
}

func TestHandlerNotFound(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/github.com/nonexistent/module/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandlerCacheHit(t *testing.T) {
	// Track upstream calls
	upstreamCalls := 0
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.info") {
			_ = json.NewEncoder(w).Encode(VersionInfo{Version: "v1.0.0"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.mod") {
			_, _ = w.Write([]byte("module test\n"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1.0.0.zip") {
			_, _ = w.Write([]byte("zip content"))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler, index, cafsStore := newTestHandlerWithComponents(t, mockUpstream.URL)

	ctx := context.Background()

	// Pre-populate cache
	zipHash, _ := cafsStore.Put(ctx, strings.NewReader("cached zip content"))
	mv := &ModuleVersion{
		Info:    VersionInfo{Version: "v1.0.0"},
		ZipHash: zipHash,
	}
	_ = index.PutModuleVersion(ctx, "github.com/cached/module", "v1.0.0", mv, []byte("module cached\n"))

	// Request cached module
	req := httptest.NewRequest(http.MethodGet, "/github.com/cached/module/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()

	initialCalls := upstreamCalls
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, initialCalls, upstreamCalls)
}

func TestHandlerZipCacheHit(t *testing.T) {
	upstreamCalls := 0
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler, index, cafsStore := newTestHandlerWithComponents(t, mockUpstream.URL)

	ctx := context.Background()

	// Pre-populate cache with zip
	zipContent := "cached zip content for testing"
	zipHash, _ := cafsStore.Put(ctx, strings.NewReader(zipContent))
	mv := &ModuleVersion{
		Info:    VersionInfo{Version: "v1.0.0"},
		ZipHash: zipHash,
	}
	_ = index.PutModuleVersion(ctx, "github.com/cached/module", "v1.0.0", mv, []byte("module cached\n"))

	// Request cached zip
	req := httptest.NewRequest(http.MethodGet, "/github.com/cached/module/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	body, _ := io.ReadAll(rec.Body)
	require.Equal(t, zipContent, string(body))
	require.Equal(t, 0, upstreamCalls)
}

func TestHandlerUppercaseModule(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the path is correctly encoded
		if strings.Contains(r.URL.Path, "!azure") {
			_ = json.NewEncoder(w).Encode(VersionInfo{Version: "v1.0.0"})
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer mockUpstream.Close()

	handler := newTestHandler(t, mockUpstream.URL)

	// Request with encoded uppercase path
	req := httptest.NewRequest(http.MethodGet, "/github.com/!azure/azure-sdk/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHandlerInvalidPath(t *testing.T) {
	handler := newTestHandler(t, "http://localhost")

	tests := []struct {
		name string
		path string
	}{
		{"no @v", "/github.com/test/module"},
		{"invalid suffix", "/github.com/test/module/@v/v1.0.0.xyz"},
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

func TestHandlerMethodNotAllowed(t *testing.T) {
	handler := newTestHandler(t, "http://localhost")

	req := httptest.NewRequest(http.MethodPost, "/github.com/test/module/@v/list", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// Helper functions

func newTestHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	handler, _, _ := newTestHandlerWithComponents(t, upstreamURL)
	return handler
}

func newTestHandlerWithComponents(t *testing.T, upstreamURL string) (*Handler, *Index, store.Store) {
	t.Helper()

	tmpDir := t.TempDir()
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)

	// Create and open BoltDB
	db := metadb.New()
	dbPath := filepath.Join(tmpDir, "metadata.db")
	err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	boltDB, ok := db.(*metadb.BoltDB)
	require.True(t, ok, "expected *metadb.BoltDB")

	// Create EnvelopeIndex instances for goproxy
	modIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "mod", 24*time.Hour)
	require.NoError(t, err)
	infoIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "info", 24*time.Hour)
	require.NoError(t, err)
	listIndex, err := metadb.NewEnvelopeIndex(boltDB, "goproxy", "list", 24*time.Hour)
	require.NoError(t, err)

	idx := NewIndex(modIndex, infoIndex, listIndex)
	cafsStore := store.NewCAFS(b)
	upstream := NewUpstream(WithUpstreamURL(upstreamURL))

	handler := NewHandler(idx, cafsStore, WithUpstream(upstream))

	return handler, idx, cafsStore
}
