package maven

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	metadataIdx, err := metadb.NewEnvelopeIndex(db, "maven", kindMetadata, 5*time.Minute)
	require.NoError(t, err)
	artifactIdx, err := metadb.NewEnvelopeIndex(db, "maven", kindArtifact, 24*time.Hour)
	require.NoError(t, err)
	idx := NewIndex(metadataIdx, artifactIdx)
	return db, idx, func() { _ = db.Close() }
}

func newTestHandler(t *testing.T, upstreamServer *httptest.Server) (*Handler, func()) {
	t.Helper()
	// Use a manual temp dir instead of t.TempDir() to avoid race with async goroutines
	tmpDir, err := os.MkdirTemp("", "maven-test-*")
	require.NoError(t, err)
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)

	opts := []UpstreamOption{}
	if upstreamServer != nil {
		opts = append(opts, WithRepositoryURL(upstreamServer.URL))
	}
	upstream := NewUpstream(opts...)

	h := NewHandler(idx, cafs, WithUpstream(upstream))
	return h, func() {
		h.Close()
		closeDB()
		_ = os.RemoveAll(tmpDir)
	}
}

func TestHandlerMetadata(t *testing.T) {
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
  </versioning>
</metadata>`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/org/example/test/maven-metadata.xml" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(metadataXML))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("get metadata", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/maven-metadata.xml", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), "<groupId>org.example</groupId>")
		require.Equal(t, "application/xml", w.Header().Get("Content-Type"))
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/com/nonexistent/artifact/maven-metadata.xml", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerMetadataChecksum(t *testing.T) {
	metadataXML := `<?xml version="1.0"?><metadata></metadata>`
	expectedSHA1 := sha1Sum([]byte(metadataXML))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/org/example/test/maven-metadata.xml" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(metadataXML))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("get sha1 checksum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/maven-metadata.xml.sha1", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, expectedSHA1, w.Body.String())
	})
}

func TestHandlerArtifact(t *testing.T) {
	artifactContent := []byte("fake-jar-content-12345678")
	artifactSHA1 := sha1Sum(artifactContent)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/example/test/1.0.0/test-1.0.0.jar":
			w.Header().Set("Content-Type", "application/java-archive")
			_, _ = w.Write(artifactContent)
		case "/org/example/test/1.0.0/test-1.0.0.jar.sha1":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(artifactSHA1))
		case "/org/example/test/1.0.0/test-1.0.0.pom":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<project></project>`))
		case "/org/example/test/1.0.0/test-1.0.0-sources.jar":
			w.Header().Set("Content-Type", "application/java-archive")
			_, _ = w.Write([]byte("sources-content"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("download jar", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/1.0.0/test-1.0.0.jar", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, string(artifactContent), w.Body.String())
		require.Equal(t, "application/java-archive", w.Header().Get("Content-Type"))
	})

	t.Run("download pom", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/1.0.0/test-1.0.0.pom", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), "<project>")
		require.Equal(t, "application/xml", w.Header().Get("Content-Type"))
	})

	t.Run("download sources jar", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/1.0.0/test-1.0.0-sources.jar", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "sources-content", w.Body.String())
	})

	t.Run("artifact not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/com/nonexistent/artifact/1.0.0/artifact-1.0.0.jar", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerArtifactChecksum(t *testing.T) {
	artifactContent := []byte("test-content")
	sha1Checksum := sha1Sum(artifactContent)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/example/test/1.0.0/test-1.0.0.jar.sha1":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(sha1Checksum))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("get sha1 checksum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/1.0.0/test-1.0.0.jar.sha1", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, sha1Checksum, w.Body.String())
	})

	t.Run("checksum not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/org/example/test/1.0.0/test-1.0.0.jar.sha256", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t, nil)
	defer cleanup()

	t.Run("POST not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/org/example/test/maven-metadata.xml", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})

	t.Run("PUT not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/org/example/test/1.0.0/test-1.0.0.jar", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

func TestParseArtifactFilename(t *testing.T) {
	tests := []struct {
		name         string
		artifactID   string
		version      string
		filename     string
		wantCoord    ArtifactCoordinate
		wantChecksum string
		wantErr      bool
	}{
		{
			name:       "simple jar",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0.jar",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Extension:  "jar",
			},
		},
		{
			name:       "jar with classifier",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0-sources.jar",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Classifier: "sources",
				Extension:  "jar",
			},
		},
		{
			name:       "pom file",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0.pom",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Extension:  "pom",
			},
		},
		{
			name:       "jar with sha1 checksum",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0.jar.sha1",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Extension:  "jar",
			},
			wantChecksum: "sha1",
		},
		{
			name:       "jar with md5 checksum",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0.jar.md5",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Extension:  "jar",
			},
			wantChecksum: "md5",
		},
		{
			name:       "classifier with checksum",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0-javadoc.jar.sha256",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "test",
				Version:    "1.0.0",
				Classifier: "javadoc",
				Extension:  "jar",
			},
			wantChecksum: "sha256",
		},
		{
			name:       "complex version",
			artifactID: "spring-boot",
			version:    "3.2.0-M1",
			filename:   "spring-boot-3.2.0-M1.jar",
			wantCoord: ArtifactCoordinate{
				ArtifactID: "spring-boot",
				Version:    "3.2.0-M1",
				Extension:  "jar",
			},
		},
		{
			name:       "invalid - wrong artifactId",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "other-1.0.0.jar",
			wantErr:    true,
		},
		{
			name:       "invalid - missing extension",
			artifactID: "test",
			version:    "1.0.0",
			filename:   "test-1.0.0",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord, checksumType, err := parseArtifactFilename(tt.artifactID, tt.version, tt.filename)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantCoord.ArtifactID, coord.ArtifactID)
			require.Equal(t, tt.wantCoord.Version, coord.Version)
			require.Equal(t, tt.wantCoord.Classifier, coord.Classifier)
			require.Equal(t, tt.wantCoord.Extension, coord.Extension)
			require.Equal(t, tt.wantChecksum, checksumType)
		})
	}
}

func TestComputeChecksum(t *testing.T) {
	data := []byte("test data")

	t.Run("sha1", func(t *testing.T) {
		result := computeChecksum(data, ChecksumSHA1)
		require.Equal(t, sha1Sum(data), result)
	})

	t.Run("md5", func(t *testing.T) {
		result := computeChecksum(data, ChecksumMD5)
		require.NotEmpty(t, result)
		require.Len(t, result, 32) // MD5 hex is 32 chars
	})

	t.Run("sha256", func(t *testing.T) {
		result := computeChecksum(data, ChecksumSHA256)
		require.NotEmpty(t, result)
		require.Len(t, result, 64) // SHA256 hex is 64 chars
	})

	t.Run("unknown type", func(t *testing.T) {
		result := computeChecksum(data, "unknown")
		require.Empty(t, result)
	})
}

func TestContentTypeForExtension(t *testing.T) {
	tests := []struct {
		ext      string
		expected string
	}{
		{ExtensionJAR, "application/java-archive"},
		{ExtensionWAR, "application/java-archive"},
		{ExtensionPOM, "application/xml"},
		{ExtensionZIP, "application/zip"},
		{"unknown", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			require.Equal(t, tt.expected, contentTypeForExtension(tt.ext))
		})
	}
}

// Helper function
func sha1Sum(data []byte) string {
	h := sha1.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
