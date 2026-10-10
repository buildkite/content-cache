package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reproduce registry metadata exceeding upstream's former 10MiB limit.
// A second request must succeed after upstream becomes unavailable.
func TestLargeNPMMetadataIsCached(t *testing.T) {
	var upstreamRequests atomic.Int32
	metadata, err := json.Marshal(map[string]any{
		"name":     "large-package",
		"readme":   strings.Repeat("x", 12*1024*1024),
		"versions": map[string]any{"1.0.0": map[string]any{"name": "large-package", "version": "1.0.0"}},
	})
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(metadata)
	}))
	defer upstream.Close()
	s, err := New(Config{
		StoragePath: t.TempDir(), UpstreamNPMRegistry: upstream.URL,
		OCIPrefix:       "docker-hub",
		MetadataMaxSize: 16 * 1024 * 1024,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.Shutdown(ctx))
	})
	first := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/npm/large-package", nil))
	require.Equal(t, http.StatusOK, first.Code)
	require.Eventually(t, func() bool {
		_, err := s.npmIndex.GetPackageMetadata(context.Background(), "large-package")
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	upstream.Close()
	second := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/npm/large-package", nil))
	require.Equal(t, http.StatusOK, second.Code)
	require.JSONEq(t, first.Body.String(), second.Body.String())
	require.Equal(t, int32(1), upstreamRequests.Load())
}

func TestMetadataLimitValidatedBeforeStorage(t *testing.T) {
	_, err := New(Config{MetadataMaxSize: -1})
	require.ErrorContains(t, err, "metadata maximum size")
}

func TestShutdownCancelsMetadataFetchAfterClientDisconnects(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{
		StoragePath: t.TempDir(), UpstreamNPMRegistry: upstream.URL,
		OCIPrefix: "docker-hub", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.Shutdown(ctx))
	})
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	done := make(chan struct{})
	go func() {
		s.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/npm/pkg", nil).WithContext(ctx))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream fetch did not start")
	}
	disconnect()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected client did not leave")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(shutdownCtx))
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown did not cancel the shared fetch")
	}
}
