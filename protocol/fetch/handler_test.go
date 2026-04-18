package fetch

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	cloneURL := *clone.URL
	clone.URL = &cloneURL
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.base.RoundTrip(clone)
}

func setupMetaDB(t *testing.T, tmpDir string) (*metadb.BoltDB, *Index) {
	t.Helper()
	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "meta.db")))
	t.Cleanup(func() { _ = db.Close() })

	resources, err := metadb.NewEnvelopeIndex(db, "fetch", "resource", 24*time.Hour)
	require.NoError(t, err)
	return db, NewIndex(resources)
}

func newTestHandler(t *testing.T, client *http.Client, opts ...HandlerOption) *Handler {
	t.Helper()
	tmpDir := t.TempDir()
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	cafs := store.NewCAFS(b)

	_, idx := setupMetaDB(t, tmpDir)
	baseOpts := []HandlerOption{}
	if client != nil {
		baseOpts = append(baseOpts, WithHTTPClient(client))
	}
	baseOpts = append(baseOpts, opts...)
	return NewHandler(idx, cafs, baseOpts...)
}

func TestHandlerFetchCachesResource(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/tools/tool/v1.0.0/tool.tar.gz" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/tools/tool/v1.0.0/tool.tar.gz")
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="tool.tar.gz"`)
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte("tool-content"))
	}))
	defer upstream.Close()

	host := upstream.Listener.Addr().String()
	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{host}))

	req := httptest.NewRequest(http.MethodGet, "/"+host+"/tools/tool/v1.0.0/tool.tar.gz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "tool-content", w.Body.String())
	require.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename="tool.tar.gz"`, w.Header().Get("Content-Disposition"))
	require.Equal(t, `"abc123"`, w.Header().Get("ETag"))

	req2 := httptest.NewRequest(http.MethodGet, "/"+host+"/tools/tool/v1.0.0/tool.tar.gz", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, "tool-content", w2.Body.String())
	require.Equal(t, 1, requests)
}

func TestHandlerGitHubReleaseCachesRedirectedAsset(t *testing.T) {
	assetRequests := 0
	assetServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assetRequests++
		if r.URL.Path != "/download/tool.tar.gz" {
			t.Errorf("asset path = %q, want %q", r.URL.Path, "/download/tool.tar.gz")
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("redirected-asset"))
	}))
	defer assetServer.Close()

	githubRequests := 0
	githubServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubRequests++
		if r.URL.Path != "/owner/repo/releases/download/v1.0.0/tool.tar.gz" {
			t.Errorf("github release path = %q, want %q", r.URL.Path, "/owner/repo/releases/download/v1.0.0/tool.tar.gz")
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, assetServer.URL+"/download/tool.tar.gz", http.StatusFound)
	}))
	defer githubServer.Close()

	pool := x509.NewCertPool()
	pool.AddCert(assetServer.Certificate())
	pool.AddCert(githubServer.Certificate())
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}},
		Timeout: defaultTimeout,
	}

	h := newTestHandler(
		t,
		client,
		WithGitHubReleaseHost(githubServer.Listener.Addr().String()),
		WithGitHubReleaseRedirectHosts(assetServer.Listener.Addr().String()),
	)

	req := httptest.NewRequest(http.MethodGet, "/owner/repo/releases/download/v1.0.0/tool.tar.gz", nil)
	w := httptest.NewRecorder()
	h.ServeGitHubRelease(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "redirected-asset", w.Body.String())
	require.Equal(t, 1, githubRequests)
	require.Equal(t, 1, assetRequests)

	req2 := httptest.NewRequest(http.MethodGet, "/owner/repo/releases/download/v1.0.0/tool.tar.gz", nil)
	w2 := httptest.NewRecorder()
	h.ServeGitHubRelease(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.Equal(t, "redirected-asset", w2.Body.String())
	require.Equal(t, 1, githubRequests)
	require.Equal(t, 1, assetRequests)
}

func TestHandlerFetchRejectsDisallowedHost(t *testing.T) {
	h := newTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/example.com/tool.tar.gz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandlerFetchRejectsUnlistedPortOnAllowedHostname(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("should-not-be-fetched"))
	}))
	defer upstream.Close()

	host, _, err := net.SplitHostPort(upstream.Listener.Addr().String())
	require.NoError(t, err)

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{host}))
	req := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Zero(t, requests)
}

func TestHandlerFetchPreservesContentEncodingOnHeadMiss(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/encoded/tool.tar.gz" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/encoded/tool.tar.gz")
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "br")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write([]byte("brotli-bytes"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))
	req := httptest.NewRequest(http.MethodHead, "/"+upstream.Listener.Addr().String()+"/encoded/tool.tar.gz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "br", w.Header().Get("Content-Encoding"))
}

func TestHandlerFetchPreservesContentEncodingOnMissAndHit(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/encoded/tool.tar.gz" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/encoded/tool.tar.gz")
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("brotli-bytes"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))

	firstReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/encoded/tool.tar.gz", nil)
	firstRes := httptest.NewRecorder()
	h.ServeHTTP(firstRes, firstReq)

	require.Equal(t, http.StatusOK, firstRes.Code)
	require.Equal(t, "br", firstRes.Header().Get("Content-Encoding"))
	require.Equal(t, "brotli-bytes", firstRes.Body.String())

	secondReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/encoded/tool.tar.gz", nil)
	secondRes := httptest.NewRecorder()
	h.ServeHTTP(secondRes, secondReq)

	require.Equal(t, http.StatusOK, secondRes.Code)
	require.Equal(t, "br", secondRes.Header().Get("Content-Encoding"))
	require.Equal(t, "brotli-bytes", secondRes.Body.String())
	require.Equal(t, 1, requests)
}

func TestHandlerFetchRequestsIdentityEncoding(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			http.Error(w, "unexpected accept-encoding: "+got, http.StatusNotAcceptable)
			return
		}
		_, _ = w.Write([]byte("identity-only"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))
	req := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/archives/tool.tar.gz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "identity-only", w.Body.String())
}

func TestHandlerFetchConditionalGetReturnsNotModifiedFromCache(t *testing.T) {
	requests := 0
	lastModified := time.Date(2026, time.April, 10, 2, 3, 4, 0, time.UTC).Format(http.TimeFormat)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", lastModified)
		_, _ = w.Write([]byte("cached-body"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))

	firstReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	firstRes := httptest.NewRecorder()
	h.ServeHTTP(firstRes, firstReq)
	require.Equal(t, http.StatusOK, firstRes.Code)
	require.Equal(t, "cached-body", firstRes.Body.String())

	etagReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	etagReq.Header.Set("If-None-Match", `W/"abc123"`)
	etagRes := httptest.NewRecorder()
	h.ServeHTTP(etagRes, etagReq)

	require.Equal(t, http.StatusNotModified, etagRes.Code)
	require.Empty(t, etagRes.Body.String())
	require.Equal(t, `"abc123"`, etagRes.Header().Get("ETag"))

	modifiedReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	modifiedReq.Header.Set("If-Modified-Since", lastModified)
	modifiedRes := httptest.NewRecorder()
	h.ServeHTTP(modifiedRes, modifiedReq)

	require.Equal(t, http.StatusNotModified, modifiedRes.Code)
	require.Empty(t, modifiedRes.Body.String())
	require.Equal(t, lastModified, modifiedRes.Header().Get("Last-Modified"))
	require.Equal(t, 1, requests)
}

func TestHandlerFetchConditionalGetForwardsValidatorsOnMiss(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("If-None-Match"); got != `"miss-etag"` {
			t.Errorf("If-None-Match = %q, want %q", got, `"miss-etag"`)
		}
		w.Header().Set("ETag", `"miss-etag"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))
	req := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	req.Header.Set("If-None-Match", `"miss-etag"`)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotModified, w.Code)
	require.Empty(t, w.Body.String())
	require.Equal(t, 1, requests)
}

func TestHandlerFetchConditionalGetWarmsCacheOnMiss(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if got := r.Header.Get("If-None-Match"); got != `"stale-etag"` {
				t.Errorf("If-None-Match = %q, want %q", got, `"stale-etag"`)
			}
		}
		w.Header().Set("ETag", `"fresh-etag"`)
		_, _ = w.Write([]byte("fresh-body"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))

	conditionalReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	conditionalReq.Header.Set("If-None-Match", `"stale-etag"`)
	conditionalRes := httptest.NewRecorder()
	h.ServeHTTP(conditionalRes, conditionalReq)

	require.Equal(t, http.StatusOK, conditionalRes.Code)
	require.Equal(t, "fresh-body", conditionalRes.Body.String())
	require.Equal(t, `"fresh-etag"`, conditionalRes.Header().Get("ETag"))

	cachedReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	cachedRes := httptest.NewRecorder()
	h.ServeHTTP(cachedRes, cachedReq)

	require.Equal(t, http.StatusOK, cachedRes.Code)
	require.Equal(t, "fresh-body", cachedRes.Body.String())
	require.Equal(t, `"fresh-etag"`, cachedRes.Header().Get("ETag"))
	require.Equal(t, 1, requests)
}

func TestHandlerFetchWildcardIfNoneMatchWithoutETagReturnsNotModified(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("cached-body"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))

	warmReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	warmRes := httptest.NewRecorder()
	h.ServeHTTP(warmRes, warmReq)
	require.Equal(t, http.StatusOK, warmRes.Code)

	conditionalReq := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	conditionalReq.Header.Set("If-None-Match", "*")
	conditionalRes := httptest.NewRecorder()
	h.ServeHTTP(conditionalRes, conditionalReq)

	require.Equal(t, http.StatusNotModified, conditionalRes.Code)
	require.Empty(t, conditionalRes.Body.String())
	require.Equal(t, 1, requests)
}

func TestHandlerFetchNormalisesEquivalentHTTPSAuthoritiesToOneCacheKey(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("cached-body"))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client := &http.Client{
		Transport: rewriteTransport{target: target, base: upstream.Client().Transport},
		Timeout:   defaultTimeout,
	}
	h := newTestHandler(t, client, WithAllowedHosts([]string{"example.com"}))

	firstReq := httptest.NewRequest(http.MethodGet, "/Example.COM:443/tool.tar.gz", nil)
	firstRes := httptest.NewRecorder()
	h.ServeHTTP(firstRes, firstReq)
	require.Equal(t, http.StatusOK, firstRes.Code)

	secondReq := httptest.NewRequest(http.MethodGet, "/example.com/tool.tar.gz", nil)
	secondRes := httptest.NewRecorder()
	h.ServeHTTP(secondRes, secondReq)

	require.Equal(t, http.StatusOK, secondRes.Code)
	require.Equal(t, "cached-body", secondRes.Body.String())
	require.Equal(t, 1, requests)
}

func TestHandlerFetchRejectsHTTPRedirects(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("insecure-body"))
	}))
	defer redirectTarget.Close()

	targetURL, err := url.Parse(redirectTarget.URL)
	require.NoError(t, err)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/tool.tar.gz", http.StatusFound)
	}))
	defer upstream.Close()

	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}},
		Timeout: defaultTimeout,
	}
	h := newTestHandler(t, client, WithAllowedHosts([]string{upstream.Listener.Addr().String(), targetURL.Host}))

	req := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "host not allowed")
}

func TestHandlerFetchRejectsRangeRequests(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("unexpected"))
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.Client(), WithAllowedHosts([]string{upstream.Listener.Addr().String()}))
	req := httptest.NewRequest(http.MethodGet, "/"+upstream.Listener.Addr().String()+"/tool.tar.gz", nil)
	req.Header.Set("Range", "bytes=0-10")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotImplemented, w.Code)
	require.Equal(t, "none", w.Header().Get("Accept-Ranges"))
	require.Zero(t, requests)
}
