package s3fifo

import (
	"context"
	"testing"

	contentcache "github.com/buildkite/content-cache"
	"github.com/stretchr/testify/require"
)

type observingEvictionContext struct {
	context.Context
	onCheck func()
}

func (c observingEvictionContext) Err() error {
	c.onCheck()
	return c.Context.Err()
}

func TestPinnedScanAllowsQueueChangesBetweenCandidates(t *testing.T) {
	for _, queue := range []string{QueueSmall, QueueMain} {
		for _, change := range []string{"admit", "admit-pinned", "remove", "cancel"} {
			t.Run(queue+"/"+change, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				mgr, mdb, b := testManager(t, Config{MaxSize: 15})
				pinned := putBlob(t, ctx, mdb, b, "pinned-000")
				candidate := putBlob(t, ctx, mdb, b, "candidate0")
				added := putBlob(t, ctx, mdb, b, "new-blob00")
				entry, err := mdb.GetBlob(ctx, pinned)
				require.NoError(t, err)
				entry.RefCount = 1
				require.NoError(t, mdb.PutBlob(ctx, entry))
				if change == "admit-pinned" {
					for _, hash := range []string{candidate, added} {
						entry, err := mdb.GetBlob(ctx, hash)
						require.NoError(t, err)
						entry.RefCount = 1
						require.NoError(t, mdb.PutBlob(ctx, entry))
					}
				}

				for _, hash := range []string{pinned, candidate} {
					pushHead(t, mgr.queues, queue, hash)
				}
				if queue == QueueSmall {
					mgr.smallBytes, mgr.smallLen = 20, 2
				} else {
					mgr.mainBytes, mgr.mainLen = 20, 2
				}

				checks := 0
				changed := false
				// Use the cancellation-check boundary to interleave an operation
				// deterministically. Checks within an atomic candidate operation
				// do not count: admissions/removals must be able to take queueMu.
				scanCtx := observingEvictionContext{Context: ctx, onCheck: func() {
					if !mgr.queueMu.TryLock() {
						return
					}
					mgr.queueMu.Unlock()
					checks++
					if checks != 2 {
						return
					}
					changed = true
					switch change {
					case "admit", "admit-pinned":
						mgr.Admit(ctx, added, 10)
					case "remove":
						mgr.Remove(ctx, pinned, 10)
					case "cancel":
						cancel()
					}
				}}
				mgr.maybeEvict(scanCtx)
				require.True(t, changed, "scan must release queueMu and check cancellation between pinned candidates")

				exists, err := b.Exists(t.Context(), contentcache.BlobStorageKey(mustParseHash(t, candidate)))
				require.NoError(t, err)
				require.Equal(t, change != "admit", exists, "stop eviction after reaching capacity or cancellation")
				state := mgr.snapshotState()
				wantBytes, wantLen := int64(20), 2
				switch change {
				case "remove":
					wantBytes, wantLen = 10, 1
				case "admit-pinned":
					wantBytes, wantLen = 30, 3
					require.Equal(t, 2, checks, "new admissions must not extend an all-pinned scan")
				}
				require.Equal(t, wantBytes, state.smallBytes+state.mainBytes)
				require.Equal(t, wantLen, state.smallLen+state.mainLen)
				for _, q := range []string{QueueSmall, QueueMain} {
					n, err := mgr.queues.Len(q)
					require.NoError(t, err)
					if q == QueueSmall {
						require.Equal(t, state.smallLen, n)
					} else {
						require.Equal(t, state.mainLen, n)
					}
				}
			})
		}
	}
}

func TestPinnedScanRechecksSmallQueueTarget(t *testing.T) {
	ctx := t.Context()
	mgr, mdb, b := testManager(t, Config{MaxSize: 25, SmallQueuePercent: 50})
	pinned := putBlob(t, ctx, mdb, b, "pinned-000")
	candidate := putBlob(t, ctx, mdb, b, "candidate0")
	main := putBlob(t, ctx, mdb, b, "main-blob-0123456789")
	entry, err := mdb.GetBlob(ctx, pinned)
	require.NoError(t, err)
	entry.RefCount = 1
	require.NoError(t, mdb.PutBlob(ctx, entry))
	pushHead(t, mgr.queues, QueueSmall, pinned)
	pushHead(t, mgr.queues, QueueSmall, candidate)
	pushHead(t, mgr.queues, QueueMain, main)
	mgr.smallBytes, mgr.smallLen = 20, 2
	mgr.mainBytes, mgr.mainLen = 20, 1

	checks := 0
	scanCtx := observingEvictionContext{Context: ctx, onCheck: func() {
		if !mgr.queueMu.TryLock() {
			return
		}
		mgr.queueMu.Unlock()
		checks++
		if checks == 2 {
			// Total remains over capacity, but small is now below its target.
			mgr.Remove(ctx, pinned, 10)
		}
	}}
	mgr.maybeEvict(scanCtx)

	exists, err := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, candidate)))
	require.NoError(t, err)
	require.True(t, exists, "eviction must switch to main after small falls below its target")
	exists, err = b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, main)))
	require.NoError(t, err)
	require.False(t, exists)
	state := mgr.snapshotState()
	require.Equal(t, int64(10), state.smallBytes)
	require.Equal(t, int64(0), state.mainBytes)
	require.Equal(t, 1, state.smallLen)
	require.Equal(t, 0, state.mainLen)
}
