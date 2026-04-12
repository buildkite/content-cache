// Package download provides singleflight-based deduplication for concurrent
// upstream fetches. When multiple requests arrive for the same uncached
// resource, only one upstream fetch is performed.
package download

import (
	"context"

	contentcache "github.com/buildkite/content-cache"
	"golang.org/x/sync/singleflight"
)

// Result holds the outcome of a download operation.
type Result struct {
	Hash contentcache.Hash
	Size int64
}

// DownloadFunc fetches from upstream, verifies integrity, and stores in CAFS.
// The context passed to DownloadFunc is detached from any single request so
// that one caller timing out does not cancel the download for other waiters.
type DownloadFunc func(ctx context.Context) (*Result, error)

// Downloader deduplicates concurrent downloads for the same resource key
// using singleflight. It uses DoChan so each caller can respect its own
// context deadline without cancelling the in-flight download for others.
type Downloader struct {
	group singleflight.Group
}

// New creates a new Downloader.
func New() *Downloader {
	return &Downloader{}
}

// Do deduplicates concurrent downloads for the same key.
// The fn receives a background context (not tied to any single request).
// Returns the result, whether it was shared with another caller, and any error.
//
// If the caller's context expires before the download completes, Do returns
// the context error but the in-flight download continues for other waiters.
func (d *Downloader) Do(ctx context.Context, key string, fn DownloadFunc) (*Result, bool, error) {
	ch := d.group.DoChan(key, func() (any, error) {
		// Use a detached context so that no single caller's cancellation
		// stops the download for everyone else.
		return fn(context.WithoutCancel(ctx))
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Shared, res.Err
		}
		return res.Val.(*Result), res.Shared, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// Forget removes the key from the singleflight group, allowing a subsequent
// call to retry. Typically called after a download error.
func (d *Downloader) Forget(key string) {
	d.group.Forget(key)
}
