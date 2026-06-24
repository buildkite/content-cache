package pypi

import (
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

// setupMetaDB creates a BoltDB with an EnvelopeIndex for testing.
// Returns the database, index, and a cleanup function that closes the database.
func setupMetaDB(t *testing.T, tmpDir string) (*metadb.BoltDB, *Index, func()) {
	t.Helper()
	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	projectIdx, err := metadb.NewEnvelopeIndex(db, "pypi", "project", 5*time.Minute)
	require.NoError(t, err)
	idx := NewIndex(projectIdx)
	return db, idx, func() { _ = db.Close() }
}

func newTestHandler(t *testing.T, upstreamServer *httptest.Server, extraOpts ...HandlerOption) (*Handler, func()) {
	t.Helper()
	// Use a manual temp dir instead of t.TempDir() to avoid race with async goroutines
	tmpDir, err := os.MkdirTemp("", "pypi-test-*")
	require.NoError(t, err)
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)

	opts := []UpstreamOption{}
	if upstreamServer != nil {
		opts = append(opts, WithSimpleURL(upstreamServer.URL+"/simple/"))
	}
	upstream := NewUpstream(opts...)

	h := NewHandler(idx, cafs, append([]HandlerOption{WithUpstream(upstream)}, extraOpts...)...)
	return h, func() {
		h.Close()
		closeDB()
		_ = os.RemoveAll(tmpDir)
	}
}

func TestHandlerProject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/requests/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Links for requests</title></head>
<body>
<h1>Links for requests</h1>
<a href="https://files.pythonhosted.org/packages/requests-2.31.0.tar.gz#sha256=942c5a758f98d790eaed1a29cb6eefc7ffb0d1cf7af05c3d2791656dbd6ad1e1">requests-2.31.0.tar.gz</a><br/>
<a href="https://files.pythonhosted.org/packages/requests-2.31.0-py3-none-any.whl#sha256=58cd2187c01e70e6e26505bca751777aa9f2ee0b7f4300988b709f44e013003f" data-requires-python=">=3.7">requests-2.31.0-py3-none-any.whl</a><br/>
</body>
</html>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("get project page HTML", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/requests/", nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		require.Contains(t, body, "requests-2.31.0")
		require.Contains(t, body, "/pypi/packages/requests/")
	})

	t.Run("get project page JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/requests/", nil)
		req.Header.Set("Accept", ContentTypeJSON)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		require.Contains(t, body, `"api-version"`)
		require.Contains(t, body, `"name":"requests"`)
	})

	t.Run("project not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/nonexistent-pkg/", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("redirect missing trailing slash", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/requests", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusMovedPermanently, w.Code)
		require.Equal(t, "/simple/requests/", w.Header().Get("Location"))
	})
}

func TestHandlerProjectPublicBaseURL(t *testing.T) {
	upstreamHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/requests/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><body>
<a href="https://files.pythonhosted.org/packages/requests-2.31.0-py3-none-any.whl#sha256=58cd2187c01e70e6e26505bca751777aa9f2ee0b7f4300988b709f44e013003f">requests-2.31.0-py3-none-any.whl</a><br/>
</body></html>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}

	t.Run("configured base URL overrides request scheme and host", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(upstreamHandler))
		defer upstream.Close()

		h, cleanup := newTestHandler(t, upstream, WithPublicBaseURL("https://cache.example.com/"))
		defer cleanup()

		req := httptest.NewRequest(http.MethodGet, "/simple/requests/", nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		// Trailing slash trimmed, configured host used, request Host ignored.
		require.Contains(t, body, "https://cache.example.com/pypi/packages/requests/")
		require.NotContains(t, body, "localhost:8080")
	})

	t.Run("falls back to request scheme and host when unset", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(upstreamHandler))
		defer upstream.Close()

		h, cleanup := newTestHandler(t, upstream)
		defer cleanup()

		req := httptest.NewRequest(http.MethodGet, "/simple/requests/", nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), "http://localhost:8080/pypi/packages/requests/")
	})
}

func TestHandlerRoot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	t.Run("empty root HTML", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), "Simple Index")
	})

	t.Run("empty root JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple/", nil)
		req.Header.Set("Accept", ContentTypeJSON)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		require.Contains(t, body, `"api-version"`)
		require.Contains(t, body, `"projects"`)
	})

	t.Run("redirect missing trailing slash", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/simple", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusMovedPermanently, w.Code)
	})
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t, nil)
	defer cleanup()

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/simple/test/", nil)
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}

func TestNormalizeProjectName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"requests", "requests"},
		{"Requests", "requests"},
		{"requests_oauthlib", "requests-oauthlib"},
		{"requests.oauthlib", "requests-oauthlib"},
		{"requests-oauthlib", "requests-oauthlib"},
		{"Django", "django"},
		{"Pillow", "pillow"},
		{"some__weird___name", "some-weird-name"},
		{"A.B_C-D", "a-b-c-d"},
		// Edge cases
		{"", ""},
		{"___", ""},
		{"-.-", ""},
		{"-foo-", "foo"},
		{"_bar_", "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeProjectName(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseProjectPageHTML(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head><title>Links for requests</title></head>
<body>
<h1>Links for requests</h1>
<a href="https://files.pythonhosted.org/packages/requests-2.31.0.tar.gz#sha256=942c5a758f">requests-2.31.0.tar.gz</a><br/>
<a href="../packages/requests-2.31.0-py3-none-any.whl#sha256=58cd2187c0" data-requires-python="&gt;=3.7">requests-2.31.0-py3-none-any.whl</a><br/>
<a href="https://example.com/yanked.whl" data-yanked="">yanked.whl</a><br/>
<a href="https://example.com/yanked-reason.whl" data-yanked="security issue">yanked-reason.whl</a><br/>
</body>
</html>`

	files, err := ParseProjectPageHTML([]byte(html), "https://pypi.org/simple/requests/")
	require.NoError(t, err)

	require.Len(t, files, 4)

	// Check first file
	require.Equal(t, "requests-2.31.0.tar.gz", files[0].Filename)
	require.Equal(t, "942c5a758f", files[0].Hashes["sha256"])

	// Check second file with requires-python
	require.Equal(t, ">=3.7", files[1].RequiresPython)

	// Check relative URL resolution
	require.True(t, strings.HasPrefix(files[1].URL, "https://pypi.org/"))

	// Check yanked files
	require.Equal(t, true, files[2].Yanked)
	require.Equal(t, "security issue", files[3].Yanked)
}

func TestParseHashFragment(t *testing.T) {
	tests := []struct {
		fragment string
		wantAlg  string
		wantHash string
	}{
		{"sha256=942c5a758f98d790", "sha256", "942c5a758f98d790"},
		{"md5=abc123def456", "md5", "abc123def456"},
		{"sha512=abcdef0123456789", "sha512", "abcdef0123456789"},
		{"invalid", "", ""},
		{"sha256=", "", ""},
		{"=abc123", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.fragment, func(t *testing.T) {
			hashes := parseHashFragment(tt.fragment)
			if tt.wantAlg == "" {
				require.Nil(t, hashes)
				return
			}
			require.NotNil(t, hashes)
			require.Equal(t, tt.wantHash, hashes[tt.wantAlg])
		})
	}
}

func TestHandlerFile(t *testing.T) {
	fileContent := []byte("fake-wheel-content-12345")

	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/test-pkg/" {
			w.Header().Set("Content-Type", "text/html")
			// Use absolute URL to upstream server for the file
			_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><body>
<a href="` + upstreamURL + `/files/test_pkg-1.0.0-py3-none-any.whl#sha256=e8d468b23a7abc7dfe5cf9832305250ad33b58a189ffa6037368d41278aab330">test_pkg-1.0.0-py3-none-any.whl</a>
</body></html>`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "test_pkg-1.0.0-py3-none-any.whl") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(fileContent)
			return
		}
		t.Logf("404 for path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()

	// Create shared storage for this test
	tmpDir, err := os.MkdirTemp("", "pypi-file-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)
	defer closeDB()

	pypiUpstream := NewUpstream(WithSimpleURL(upstream.URL + "/simple/"))
	h := NewHandler(idx, cafs, WithUpstream(pypiUpstream))

	// First fetch the project page to populate the index
	req := httptest.NewRequest(http.MethodGet, "/simple/test-pkg/", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Wait for async caching to complete
	h.Close()

	// Create new handler with same storage
	h2 := NewHandler(idx, cafs, WithUpstream(pypiUpstream))
	defer h2.Close()

	t.Run("download file", func(t *testing.T) {
		// Note: In real usage, /pypi prefix is stripped by server, so handler sees /packages/...
		req := httptest.NewRequest(http.MethodGet, "/packages/test-pkg/test_pkg-1.0.0-py3-none-any.whl", nil)
		w := httptest.NewRecorder()

		h2.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, string(fileContent), w.Body.String())
	})

	t.Run("file not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/packages/nonexistent/file.whl", nil)
		w := httptest.NewRecorder()

		h2.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlerIntegrityCheckFailure(t *testing.T) {
	// Upstream returns file with wrong content (hash won't match)
	wrongContent := []byte("this-is-wrong-content")
	expectedHash := "e8d468b23a7abc7dfe5cf9832305250ad33b58a189ffa6037368d41278aab330" // hash of different content

	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/bad-pkg/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><body>
<a href="` + upstreamURL + `/files/bad_pkg-1.0.0.whl#sha256=` + expectedHash + `">bad_pkg-1.0.0.whl</a>
</body></html>`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "bad_pkg-1.0.0.whl") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(wrongContent) // Wrong content!
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()

	tmpDir, err := os.MkdirTemp("", "pypi-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)
	defer closeDB()

	pypiUpstream := NewUpstream(WithSimpleURL(upstream.URL + "/simple/"))
	h := NewHandler(idx, cafs, WithUpstream(pypiUpstream))

	// First fetch project page to populate index
	req := httptest.NewRequest(http.MethodGet, "/simple/bad-pkg/", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Wait for async caching to complete
	h.Close()

	// Create new handler with same storage for file download
	h2 := NewHandler(idx, cafs, WithUpstream(pypiUpstream))
	defer h2.Close()

	// Now try to download the file - should fail integrity check
	req = httptest.NewRequest(http.MethodGet, "/packages/bad-pkg/bad_pkg-1.0.0.whl", nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "integrity check failed")
}

func TestHandlerMalformedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/malformed-json/" {
			w.Header().Set("Content-Type", ContentTypeJSON)
			_, _ = w.Write([]byte(`{"invalid json`)) // Malformed JSON
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h, cleanup := newTestHandler(t, upstream)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/simple/malformed-json/", nil)
	req.Header.Set("Accept", ContentTypeJSON)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadGateway, w.Code)
}

func TestHandlerHEADRequest(t *testing.T) {
	fileContent := []byte("test-file-content-for-head")
	// SHA256 of fileContent
	fileHash := "f81b8bf17fad7a3b7885eb526b4efc62ed2843eb52514eb76b59936c527ad6cf"

	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/head-test/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><body>
<a href="` + upstreamURL + `/files/head_test-1.0.0.whl#sha256=` + fileHash + `">head_test-1.0.0.whl</a>
</body></html>`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "head_test-1.0.0.whl") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(fileContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()

	tmpDir, err := os.MkdirTemp("", "pypi-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx, closeDB := setupMetaDB(t, tmpDir)
	defer closeDB()

	pypiUpstream := NewUpstream(WithSimpleURL(upstream.URL + "/simple/"))
	h := NewHandler(idx, cafs, WithUpstream(pypiUpstream))

	t.Run("HEAD on project page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/simple/head-test/", nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Zero(t, w.Body.Len())
	})

	// Fetch project page first to populate index for file test
	req := httptest.NewRequest(http.MethodGet, "/simple/head-test/", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Wait for async caching to complete
	h.Close()

	// Create new handler with same storage for file download test
	h2 := NewHandler(idx, cafs, WithUpstream(pypiUpstream))
	defer h2.Close()

	t.Run("HEAD on file download", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/packages/head-test/head_test-1.0.0.whl", nil)
		w := httptest.NewRecorder()

		h2.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotEmpty(t, w.Header().Get("Content-Length"))
		require.Zero(t, w.Body.Len())
	})
}
