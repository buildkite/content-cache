package download

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/stretchr/testify/require"
)

func TestDo_SingleCall(t *testing.T) {
	d := New()

	expected := &Result{
		Hash: contentcache.HashBytes([]byte("hello")),
		Size: 5,
	}

	result, shared, err := d.Do(context.Background(), "key1", func(ctx context.Context) (*Result, error) {
		return expected, nil
	})

	require.NoError(t, err)
	require.False(t, shared)
	require.Equal(t, expected.Hash, result.Hash)
	require.Equal(t, expected.Size, result.Size)
}

func TestDo_ConcurrentDeduplication(t *testing.T) {
	d := New()

	var callCount atomic.Int32
	expected := &Result{
		Hash: contentcache.HashBytes([]byte("data")),
		Size: 4,
	}

	var wg sync.WaitGroup
	results := make([]*Result, 10)
	errs := make([]error, 10)

	// Start the download function but make it slow enough for all goroutines to pile up
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], _, errs[idx] = d.Do(context.Background(), "shared-key", func(ctx context.Context) (*Result, error) {
				callCount.Add(1)
				time.Sleep(50 * time.Millisecond)
				return expected, nil
			})
		}(i)
	}

	wg.Wait()

	require.Equal(t, int32(1), callCount.Load(), "download func should be called exactly once")
	for i := range 10 {
		require.NoError(t, errs[i])
		require.Equal(t, expected.Hash, results[i].Hash)
	}
}

func TestDo_CallerTimeout(t *testing.T) {
	d := New()

	var downloadCompleted atomic.Bool
	expected := &Result{
		Hash: contentcache.HashBytes([]byte("slow")),
		Size: 4,
	}

	// First caller with short timeout
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer shortCancel()

	// Start a slow download
	var slowWg sync.WaitGroup
	slowWg.Add(1)
	go func() {
		defer slowWg.Done()
		_, _, _ = d.Do(shortCtx, "timeout-key", func(ctx context.Context) (*Result, error) {
			time.Sleep(200 * time.Millisecond)
			downloadCompleted.Store(true)
			return expected, nil
		})
	}()

	// Wait for first caller to start the download
	time.Sleep(5 * time.Millisecond)

	// Second caller with long timeout should get the result
	longCtx, longCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer longCancel()

	result, shared, err := d.Do(longCtx, "timeout-key", func(ctx context.Context) (*Result, error) {
		t.Fatal("should not be called - download already in flight")
		return nil, nil
	})

	require.NoError(t, err)
	require.True(t, shared)
	require.Equal(t, expected.Hash, result.Hash)
	require.True(t, downloadCompleted.Load())

	slowWg.Wait()
}

func TestDo_DownloadError(t *testing.T) {
	d := New()

	expectedErr := errors.New("upstream unavailable")

	var wg sync.WaitGroup
	errs := make([]error, 5)

	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, errs[idx] = d.Do(context.Background(), "error-key", func(ctx context.Context) (*Result, error) {
				time.Sleep(20 * time.Millisecond)
				return nil, expectedErr
			})
		}(i)
	}

	wg.Wait()

	for i := range 5 {
		require.ErrorIs(t, errs[i], expectedErr)
	}
}

func TestDo_DifferentKeys(t *testing.T) {
	d := New()

	var callCount atomic.Int32
	errs := make([]error, 5)
	var wg sync.WaitGroup

	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := "key-" + string(rune('a'+idx)) //nolint:gosec // bounded: idx in [0,5)
			_, _, errs[idx] = d.Do(context.Background(), key, func(ctx context.Context) (*Result, error) {
				callCount.Add(1)
				return &Result{Hash: contentcache.HashBytes([]byte(key)), Size: 1}, nil
			})
		}(i)
	}

	wg.Wait()

	for i := range 5 {
		require.NoError(t, errs[i])
	}
	require.Equal(t, int32(5), callCount.Load(), "each key should trigger its own download")
}

func TestForgetOnDownloadError_SkipsContextErrors(t *testing.T) {
	d := New()

	var callCount atomic.Int32
	expected := &Result{
		Hash: contentcache.HashBytes([]byte("data")),
		Size: 4,
	}

	// Start a slow download
	started := make(chan struct{})
	go func() {
		_, _, _ = d.Do(context.Background(), "forget-test", func(ctx context.Context) (*Result, error) {
			callCount.Add(1)
			close(started)
			time.Sleep(200 * time.Millisecond)
			return expected, nil
		})
	}()

	// Wait for download to start
	<-started

	// Simulate a caller that timed out — forgetOnDownloadError should NOT forget
	forgetOnDownloadError(d, "forget-test", context.DeadlineExceeded)

	// A new caller should still join the in-flight download (not start a new one)
	result, shared, err := d.Do(context.Background(), "forget-test", func(ctx context.Context) (*Result, error) {
		callCount.Add(1)
		return expected, nil
	})

	require.NoError(t, err)
	require.True(t, shared, "should share the in-flight download")
	require.Equal(t, expected.Hash, result.Hash)
	require.Equal(t, int32(1), callCount.Load(), "download func should be called exactly once")
}

func TestForgetOnDownloadError_ForgetsRealErrors(t *testing.T) {
	d := New()

	var callCount atomic.Int32
	expectedErr := errors.New("upstream error")

	// First call fails
	_, _, err := d.Do(context.Background(), "forget-err", func(ctx context.Context) (*Result, error) {
		callCount.Add(1)
		return nil, expectedErr
	})
	require.ErrorIs(t, err, expectedErr)

	// forgetOnDownloadError should forget since it's a real error
	forgetOnDownloadError(d, "forget-err", expectedErr)

	// Now a retry should trigger a new download
	expected := &Result{
		Hash: contentcache.HashBytes([]byte("retry")),
		Size: 5,
	}
	result, shared, err := d.Do(context.Background(), "forget-err", func(ctx context.Context) (*Result, error) {
		callCount.Add(1)
		return expected, nil
	})
	require.NoError(t, err)
	require.False(t, shared)
	require.Equal(t, expected.Hash, result.Hash)
	require.Equal(t, int32(2), callCount.Load())
}

func TestDo_Forget(t *testing.T) {
	d := New()

	expectedErr := errors.New("transient error")
	var callCount atomic.Int32

	// First call fails
	_, _, err := d.Do(context.Background(), "retry-key", func(ctx context.Context) (*Result, error) {
		callCount.Add(1)
		return nil, expectedErr
	})
	require.ErrorIs(t, err, expectedErr)
	require.Equal(t, int32(1), callCount.Load())

	// Forget the key to allow retry
	d.Forget("retry-key")

	// Second call succeeds
	expected := &Result{
		Hash: contentcache.HashBytes([]byte("retry-success")),
		Size: 13,
	}
	result, _, err := d.Do(context.Background(), "retry-key", func(ctx context.Context) (*Result, error) {
		callCount.Add(1)
		return expected, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), callCount.Load())
	require.Equal(t, expected.Hash, result.Hash)
}
