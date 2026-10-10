package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
	"github.com/buildkite/content-cache/telemetry"
	"github.com/stretchr/testify/require"
)

// Signal when a request starts waiting on metadata, so cohorts really overlap
// without sleeps or racing reads of handler internals.
type metadataWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *metadataWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type pendingMetadataRequest struct {
	r       *http.Request
	w       *httptest.ResponseRecorder
	waiting <-chan struct{}
	done    <-chan struct{}
}

func requestMetadata(h *Handler, ctx context.Context, target string, abbreviated bool) pendingMetadataRequest {
	c := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
	r := telemetry.InjectTags(httptest.NewRequest(http.MethodGet, target, nil).WithContext(c))
	if abbreviated {
		r.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, r)
		close(done)
	}()
	return pendingMetadataRequest{r: r, w: w, waiting: c.waiting, done: done}
}

func awaitMetadata(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata operation did not finish")
	}
}

func metadataGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return gate, open
}

func TestMetadataConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	gate, open := metadataGate(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"name":"@babel/core","description":"full description","versions":{"1.0.0":{"name":"@babel/core","dist":{"tarball":"https://registry.npmjs.org/@babel/core/-/core-1.0.0.tgz"}}}}`))
	}))
	t.Cleanup(upstream.Close)
	h, cleanup := newTestHandler(t, upstream)
	t.Cleanup(cleanup)

	const clients = 32
	requests := make([]pendingMetadataRequest, clients)
	for i := range requests {
		scheme := "http"
		if i%2 != 0 {
			scheme = "https"
		}
		requests[i] = requestMetadata(h, context.Background(), fmt.Sprintf("%s://cache-%d.example/@babel%%2fcore", scheme, i), i%2 == 0)
		awaitMetadata(t, requests[i].waiting)
	}
	open()
	coalesced := 0
	for i, req := range requests {
		awaitMetadata(t, req.done)
		require.Equal(t, http.StatusOK, req.w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(req.w.Body.Bytes(), &body))
		if i%2 == 0 {
			require.NotContains(t, body, "description")
		} else {
			require.Equal(t, "full description", body["description"])
		}
		versions := body["versions"].(map[string]any)
		dist := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)
		require.Equal(t, fmt.Sprintf("%s://%s/npm/@babel%%2fcore/-/core-1.0.0.tgz", req.r.URL.Scheme, req.r.Host), dist["tarball"])
		if telemetry.GetTags(req.r).CacheResult == telemetry.CacheCoalesced {
			coalesced++
		}
	}
	require.Equal(t, int32(1), calls.Load(), "32 simultaneous clients must fetch upstream once")
	require.Equal(t, clients-1, coalesced)
	// A response is released only after the write, leaving no asynchronous gap.
	cached, err := h.index.GetPackageMetadata(context.Background(), "@babel/core")
	require.NoError(t, err)
	require.Contains(t, string(cached), "https://registry.npmjs.org/")
	followup := requestMetadata(h, context.Background(), "http://other.example/@babel%2fcore", false)
	awaitMetadata(t, followup.done)
	require.Equal(t, telemetry.CacheHit, telemetry.GetTags(followup.r).CacheResult)
	require.Equal(t, int32(1), calls.Load())
}

func TestMetadataDifferentPackagesFetchConcurrently(t *testing.T) {
	gate, open := metadataGate(t)
	started := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintf(w, `{"name":%q,"versions":{}}`, r.URL.Path[1:])
	}))
	t.Cleanup(upstream.Close)
	h, cleanup := newTestHandler(t, upstream)
	t.Cleanup(cleanup)
	a := requestMetadata(h, context.Background(), "/alpha", false)
	b := requestMetadata(h, context.Background(), "/beta", false)
	paths := make([]string, 0, 2)
	for range 2 {
		select {
		case name := <-started:
			paths = append(paths, name)
		case <-time.After(5 * time.Second):
			t.Fatal("different packages were serialized")
		}
	}
	require.ElementsMatch(t, []string{"/alpha", "/beta"}, paths)
	open()
	for _, req := range []pendingMetadataRequest{a, b} {
		awaitMetadata(t, req.done)
		require.Equal(t, http.StatusOK, req.w.Code)
	}
}

func TestMetadataCancelledClientDoesNotCancelOthers(t *testing.T) {
	for _, cancelIndex := range []int{0, 1} {
		t.Run(fmt.Sprintf("cancel_client_%d", cancelIndex), func(t *testing.T) {
			var calls atomic.Int32
			gate, open := metadataGate(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write([]byte(`{"name":"pkg","versions":{}}`))
			}))
			t.Cleanup(upstream.Close)
			h, cleanup := newTestHandler(t, upstream)
			t.Cleanup(cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := make([]pendingMetadataRequest, 2)
			for i := range requests {
				requestCtx := context.Background()
				if i == cancelIndex {
					requestCtx = ctx
				}
				requests[i] = requestMetadata(h, requestCtx, "/pkg", false)
				awaitMetadata(t, requests[i].waiting)
			}
			cancel()
			awaitMetadata(t, requests[cancelIndex].done)
			require.Empty(t, requests[cancelIndex].w.Body.String())
			open()
			other := requests[1-cancelIndex]
			awaitMetadata(t, other.done)
			require.Equal(t, http.StatusOK, other.w.Code)
			require.Contains(t, other.w.Body.String(), `"name":"pkg"`)
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestMetadataFailedFetchIsSharedAndRetried(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(fmt.Sprintf("upstream_%d", status), func(t *testing.T) {
			var calls atomic.Int32
			gate, open := metadataGate(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if call == 1 {
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte("invalid JSON"))
					return
				}
				_, _ = w.Write([]byte(`{"name":"pkg","versions":{}}`))
			}))
			t.Cleanup(upstream.Close)
			h, cleanup := newTestHandler(t, upstream)
			t.Cleanup(cleanup)
			requests := make([]pendingMetadataRequest, 8)
			for i := range requests {
				requests[i] = requestMetadata(h, context.Background(), "/pkg", false)
				awaitMetadata(t, requests[i].waiting)
			}
			open()
			want := http.StatusBadGateway
			if status == http.StatusNotFound {
				want = http.StatusNotFound
			}
			for _, req := range requests {
				awaitMetadata(t, req.done)
				require.Equal(t, want, req.w.Code)
			}
			require.Equal(t, int32(1), calls.Load())
			retry := requestMetadata(h, context.Background(), "/pkg", false)
			awaitMetadata(t, retry.done)
			require.Equal(t, http.StatusOK, retry.w.Code)
			require.Equal(t, int32(2), calls.Load(), "failed flights must not poison later requests")
		})
	}
}

func TestMetadataCloseCancelsFetchAfterClientLeaves(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(upstream.Close)
	h, cleanup := newTestHandler(t, upstream)
	t.Cleanup(cleanup)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := requestMetadata(h, ctx, "/pkg", false)
	awaitMetadata(t, started)
	cancel()
	awaitMetadata(t, request.done)
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	awaitMetadata(t, closed)
	awaitMetadata(t, cancelled)
	_, _, err := h.startMetadataFetch("another-package")
	require.ErrorIs(t, err, context.Canceled)
}

func TestMetadataCacheWriteFailureStillServesWaiters(t *testing.T) {
	var calls atomic.Int32
	gate, open := metadataGate(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"name":"pkg","versions":{}}`))
	}))
	t.Cleanup(upstream.Close)
	h, cleanup := newTestHandler(t, upstream)
	t.Cleanup(cleanup)
	// An entry larger than the configured limit cannot be cached.
	codec, err := metadb.NewEnvelopeCodec(metadb.WithMaxPayloadSize(1))
	require.NoError(t, err)
	t.Cleanup(codec.Close)
	db, _, closeDB := setupMetaDB(t, t.TempDir())
	t.Cleanup(closeDB)
	h.index.metadataIndex, err = metadb.NewEnvelopeIndex(db, "npm", "metadata", time.Hour, metadb.WithEnvelopeIndexCodec(codec))
	require.NoError(t, err)
	requests := make([]pendingMetadataRequest, 4)
	for i := range requests {
		requests[i] = requestMetadata(h, context.Background(), "/pkg", false)
		awaitMetadata(t, requests[i].waiting)
	}
	open()
	for _, req := range requests {
		awaitMetadata(t, req.done)
		require.Equal(t, http.StatusOK, req.w.Code)
		require.Contains(t, req.w.Body.String(), `"name":"pkg"`)
	}
	require.Equal(t, int32(1), calls.Load())
	_, err = h.index.GetPackageMetadata(context.Background(), "pkg")
	require.ErrorIs(t, err, ErrNotFound)
	retry := requestMetadata(h, context.Background(), "/pkg", false)
	awaitMetadata(t, retry.done)
	require.Equal(t, http.StatusOK, retry.w.Code)
	require.Equal(t, int32(2), calls.Load())
}
