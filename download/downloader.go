// Package download provides singleflight-based deduplication for concurrent
// upstream fetches. When multiple requests arrive for the same uncached
// resource, only one upstream fetch is performed.
package download

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/telemetry"
	"golang.org/x/sync/singleflight"
)

const (
	spoolRoleOrigin    = "origin"
	spoolRoleCoalesced = "coalesced"
)

type spoolRecorder func(ctx context.Context, role, outcome string, duration time.Duration, bytesSaved int64)

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
	group       singleflight.Group
	recordSpool spoolRecorder
}

// New creates a new Downloader.
func New() *Downloader {
	return &Downloader{recordSpool: telemetry.RecordSpoolRequest}
}

// Do deduplicates concurrent downloads for the same key.
// The fn receives a background context (not tied to any single request).
// Returns the result, whether it was shared with another caller, and any error.
//
// If the caller's context expires before the download completes, Do returns
// the context error but the in-flight download continues for other waiters.
func (d *Downloader) Do(ctx context.Context, key string, fn DownloadFunc) (*Result, bool, error) {
	started := time.Now()
	var executed atomic.Bool
	ch := d.group.DoChan(key, func() (any, error) {
		executed.Store(true)
		// Use a detached context so that no single caller's cancellation
		// stops the download for everyone else.
		return fn(context.WithoutCancel(ctx))
	})

	select {
	case res := <-ch:
		role := spoolRoleCoalesced
		if executed.Load() {
			role = spoolRoleOrigin
		}
		outcome := spoolOutcome(res.Err)
		var bytesSaved int64
		if role == spoolRoleCoalesced && res.Err == nil {
			bytesSaved = res.Val.(*Result).Size
		}
		d.record(ctx, role, outcome, time.Since(started), bytesSaved)
		if res.Err != nil {
			return nil, res.Shared, res.Err
		}
		return res.Val.(*Result), res.Shared, nil
	case <-ctx.Done():
		role := spoolRoleCoalesced
		if executed.Load() {
			role = spoolRoleOrigin
		}
		d.record(ctx, role, spoolOutcome(ctx.Err()), time.Since(started), 0)
		return nil, false, ctx.Err()
	}
}

func (d *Downloader) record(ctx context.Context, role, outcome string, duration time.Duration, bytesSaved int64) {
	if d.recordSpool != nil {
		d.recordSpool(ctx, role, outcome, duration, bytesSaved)
	}
}

func spoolOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

// Forget removes the key from the singleflight group, allowing a subsequent
// call to retry. Typically called after a download error.
func (d *Downloader) Forget(key string) {
	d.group.Forget(key)
}
