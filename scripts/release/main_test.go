package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	var checksums strings.Builder
	for _, name := range []string{"content-cache_linux_amd64", "content-cache_linux_arm64", "LICENSE", "BUILD-INFO.txt"} {
		data := []byte("fixture-" + name)
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0600))
		fmt.Fprintf(&checksums, "%x  %s\n", sha256.Sum256(data), name)
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(checksums.String()), 0600))
	return directory
}

func TestPublishRetryAndImmutableRelease(t *testing.T) {
	files := map[string][]byte{}
	created, uploads, transient := 0, 0, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "job-token-fixture", r.Header.Get("JOB-TOKEN"))
		if strings.Contains(r.URL.Path, "/packages/generic/") {
			if transient {
				transient = false
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if r.Method == http.MethodGet {
				data, ok := files[r.URL.Path]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write(data)
				return
			}
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			files[r.URL.Path] = data
			uploads++
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == http.MethodGet {
			if created == 0 {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		created++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	p := publisher{client: server.Client(), api: server.URL + "/api/v4", project: "24", token: "job-token-fixture"}
	directory := fixture(t)
	require.NoError(t, p.publish("v1.4.0-campbox.1", directory))
	require.NoError(t, p.publish("v1.4.0-campbox.1", directory))
	require.Equal(t, 5, uploads)
	require.Equal(t, 1, created)
	files["/api/v4/projects/24/packages/generic/content-cache/1.4.0-campbox.1/content-cache_linux_amd64"] = []byte("different release")
	require.ErrorContains(t, p.publish("v1.4.0-campbox.1", directory), "different contents")
	require.Equal(t, 5, uploads)
}

func TestPublishRejectsInvalidInputBeforeNetwork(t *testing.T) {
	p := publisher{}
	directory := fixture(t)
	require.Error(t, p.publish("v1.4.0", directory))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "content-cache_linux_amd64"), []byte("corrupt"), 0600))
	require.ErrorContains(t, p.publish("v1.4.0-campbox.1", directory), "checksum mismatch")
}
