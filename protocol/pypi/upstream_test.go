package pypi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamAppliesBasicAuth(t *testing.T) {
	const (
		user = "pyx-user"
		pass = "pyx-secret"
	)

	var gotIndexAuth, gotFileAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/simple/requests/":
			gotIndexAuth = true
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body></body></html>`))
		case "/files/requests-2.31.0.whl":
			gotFileAuth = true
			_, _ = w.Write([]byte("wheel-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Credentials are supplied via userinfo, as the deployment does.
	up := NewUpstream(WithSimpleURL(srv.URL + "/simple/"))
	up.username = user
	up.password = pass

	ctx := context.Background()

	_, _, err := up.FetchProjectPage(ctx, "requests")
	require.NoError(t, err)
	require.True(t, gotIndexAuth, "index fetch should send Basic auth")

	rc, err := up.FetchFile(ctx, srv.URL+"/files/requests-2.31.0.whl")
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "wheel-bytes", string(body))
	require.True(t, gotFileAuth, "file fetch should send Basic auth")
}

func TestUpstreamScopesBasicAuthToConfiguredOrigin(t *testing.T) {
	up := NewUpstream(WithSimpleURL("https://pyx-user:pyx-secret@index.example.com:8443/simple/"))

	indexReq, err := http.NewRequest(http.MethodGet, "https://index.example.com:8443/simple/requests/", nil)
	require.NoError(t, err)
	up.setAuth(indexReq)
	_, _, ok := indexReq.BasicAuth()
	require.True(t, ok, "same-origin index fetch should send Basic auth")

	fileReq, err := http.NewRequest(http.MethodGet, "https://files.example.com:8443/files/requests-2.31.0.whl", nil)
	require.NoError(t, err)
	up.setAuth(fileReq)
	_, _, ok = fileReq.BasicAuth()
	require.False(t, ok, "cross-host file fetch should not send Basic auth")

	httpReq, err := http.NewRequest(http.MethodGet, "http://index.example.com:8443/files/requests-2.31.0.whl", nil)
	require.NoError(t, err)
	up.setAuth(httpReq)
	_, _, ok = httpReq.BasicAuth()
	require.False(t, ok, "different-scheme file fetch should not send Basic auth")

	portReq, err := http.NewRequest(http.MethodGet, "https://index.example.com:9443/files/requests-2.31.0.whl", nil)
	require.NoError(t, err)
	up.setAuth(portReq)
	_, _, ok = portReq.BasicAuth()
	require.False(t, ok, "different-port file fetch should not send Basic auth")
}

func TestUpstreamStripsBasicAuthOnCrossOriginRedirect(t *testing.T) {
	const (
		user = "pyx-user"
		pass = "pyx-secret"
	)

	var targetGotAuth atomic.Bool
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, ok := r.BasicAuth()
		targetGotAuth.Store(ok)
		_, _ = w.Write([]byte("wheel-bytes"))
	}))
	defer targetSrv.Close()

	indexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, targetSrv.URL+"/files/requests-2.31.0.whl", http.StatusFound)
	}))
	defer indexSrv.Close()

	up := NewUpstream(WithSimpleURL(indexSrv.URL + "/simple/"))
	up.username = user
	up.password = pass

	body, _, err := up.FetchProjectPage(context.Background(), "requests")
	require.NoError(t, err)
	require.Equal(t, "wheel-bytes", string(body))
	require.False(t, targetGotAuth.Load(), "redirect target should not receive Basic auth")
}

func TestWithSimpleURLStripsUserinfo(t *testing.T) {
	up := NewUpstream(WithSimpleURL("https://user:pw@api.example.com/simple/ramp/pypi/"))
	require.Equal(t, "user", up.username)
	require.Equal(t, "pw", up.password)
	require.Equal(t, "https", up.upstreamScheme)
	require.Equal(t, "api.example.com:443", up.upstreamHost)
	// Userinfo must not remain in the base URL (it would leak in logs/metrics).
	require.Equal(t, "https://api.example.com/simple/ramp/pypi/", up.baseURL)
}
