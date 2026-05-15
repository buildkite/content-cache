package oci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamCheckVersion(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/" {
				t.Errorf("path = %q, want /v2/", r.URL.Path)
			}
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		u := NewUpstream(WithRegistryURL(server.URL))
		err := u.CheckVersion(context.Background())
		require.NoError(t, err)
	})

	t.Run("auth required returns no error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		u := NewUpstream(WithRegistryURL(server.URL))
		err := u.CheckVersion(context.Background())
		require.NoError(t, err)
	})
}

func TestUpstreamFetchManifest(t *testing.T) {
	manifestContent := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	manifestDigest := "sha256:abc123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/alpine/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write([]byte(manifestContent))
			return
		}
		if r.URL.Path == "/v2/library/notfound/manifests/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u := NewUpstream(WithRegistryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		content, mediaType, digest, err := u.FetchManifest(context.Background(), "library/alpine", "latest")
		require.NoError(t, err)
		require.Equal(t, manifestContent, string(content))
		require.Equal(t, "application/vnd.oci.image.manifest.v1+json", mediaType)
		require.Equal(t, manifestDigest, digest)
	})

	t.Run("not found", func(t *testing.T) {
		_, _, _, err := u.FetchManifest(context.Background(), "library/notfound", "latest")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamHeadManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		if r.URL.Path == "/v2/library/alpine/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abc123")
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	u := NewUpstream(WithRegistryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		digest, size, mediaType, err := u.HeadManifest(context.Background(), "library/alpine", "latest")
		require.NoError(t, err)
		require.Equal(t, "sha256:abc123", digest)
		require.Equal(t, int64(1024), size)
		require.Equal(t, "application/vnd.oci.image.manifest.v1+json", mediaType)
	})

	t.Run("not found", func(t *testing.T) {
		_, _, _, err := u.HeadManifest(context.Background(), "library/notfound", "latest")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamFetchBlob(t *testing.T) {
	blobContent := []byte("blob content here")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/alpine/blobs/sha256:abc123" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(blobContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	u := NewUpstream(WithRegistryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		rc, size, err := u.FetchBlob(context.Background(), "library/alpine", "sha256:abc123")
		require.NoError(t, err)
		defer func() { _ = rc.Close() }()

		content, _ := io.ReadAll(rc)
		require.Equal(t, string(blobContent), string(content))
		_ = size // size may be -1 if Content-Length not set
	})

	t.Run("not found", func(t *testing.T) {
		_, _, err := u.FetchBlob(context.Background(), "library/alpine", "sha256:notfound")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamHeadBlob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		if r.URL.Path == "/v2/library/alpine/blobs/sha256:abc123" {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	u := NewUpstream(WithRegistryURL(server.URL))

	t.Run("success", func(t *testing.T) {
		size, err := u.HeadBlob(context.Background(), "library/alpine", "sha256:abc123")
		require.NoError(t, err)
		require.Equal(t, int64(4096), size)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := u.HeadBlob(context.Background(), "library/alpine", "sha256:notfound")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUpstreamWithAuth(t *testing.T) {
	authCallCount := 0

	// Auth server
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCallCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{ //nolint:gosec // test mock token response
			Token:     "test-token",
			ExpiresIn: 300,
		})
	}))
	defer authServer.Close()

	// Registry server that requires auth
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+authServer.URL+`",service="test",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/library/alpine/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer registry.Close()

	u := NewUpstream(WithRegistryURL(registry.URL))

	// First request should trigger auth
	_, _, _, err := u.FetchManifest(context.Background(), "library/alpine", "latest")
	require.NoError(t, err)
	require.Equal(t, 1, authCallCount)

	// Second request should use cached token
	_, _, _, err = u.FetchManifest(context.Background(), "library/alpine", "latest")
	require.NoError(t, err)
	require.Equal(t, 1, authCallCount)
}

func TestUpstreamOptions(t *testing.T) {
	t.Run("default registry", func(t *testing.T) {
		u := NewUpstream()
		require.Equal(t, DefaultRegistryURL, u.baseURL)
	})

	t.Run("custom registry", func(t *testing.T) {
		u := NewUpstream(WithRegistryURL("https://gcr.io"))
		require.Equal(t, "https://gcr.io", u.baseURL)
	})

	t.Run("trailing slash removed", func(t *testing.T) {
		u := NewUpstream(WithRegistryURL("https://gcr.io/"))
		require.Equal(t, "https://gcr.io", u.baseURL)
	})

	t.Run("basic auth", func(t *testing.T) {
		u := NewUpstream(WithBasicAuth("user", "pass"))
		require.Equal(t, "user", u.username)
		require.Equal(t, "pass", u.password)
	})
}
