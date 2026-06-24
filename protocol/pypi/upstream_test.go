package pypi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestWithSimpleURLStripsUserinfo(t *testing.T) {
	up := NewUpstream(WithSimpleURL("https://user:pw@api.example.com/simple/ramp/pypi/"))
	require.Equal(t, "user", up.username)
	require.Equal(t, "pw", up.password)
	// Userinfo must not remain in the base URL (it would leak in logs/metrics).
	require.Equal(t, "https://api.example.com/simple/ramp/pypi/", up.baseURL)
}
