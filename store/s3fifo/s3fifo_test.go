package s3fifo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

// testManager creates a Manager with a real bbolt DB, a real BoltDB MetaDB,
// and a real filesystem backend (all in a temp dir).
func testManager(t *testing.T, cfg Config) (*Manager, metadb.MetaDB, backend.Backend) {
	t.Helper()

	dir := t.TempDir()

	mdb := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, mdb.Open(dir+"/meta.db"))
	t.Cleanup(func() { mdb.Close() })

	fsBackend, err := backend.NewFilesystem(dir + "/blobs")
	require.NoError(t, err)

	if cfg.MaxSize == 0 {
		cfg.MaxSize = 10 * 1024 * 1024 // 10 MB default
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	boltQueues, err := NewBoltQueues(mdb.DB())
	require.NoError(t, err)

	mgr, err := NewManager(boltQueues, mdb, fsBackend, cfg)
	require.NoError(t, err)
	return mgr, mdb, fsBackend
}

// putBlob writes a fake blob into both the backend and MetaDB, returning its hash string.
func putBlob(t *testing.T, ctx context.Context, mdb metadb.MetaDB, b backend.Backend, content string) string {
	t.Helper()
	hr := contentcache.NewHashingReader(strings.NewReader(content))
	_, err := io.Copy(io.Discard, hr)
	require.NoError(t, err)
	hash := hr.Sum()
	key := contentcache.BlobStorageKey(hash)

	require.NoError(t, b.Write(ctx, key, strings.NewReader(content)))

	blobRef := contentcache.NewBlobRef(hash).String()
	entry := &metadb.BlobEntry{
		Hash:        blobRef,
		Size:        int64(len(content)),
		CachedAt:    time.Now(),
		LastAccess:  time.Now(),
		AccessCount: 0,
	}
	require.NoError(t, mdb.PutBlob(ctx, entry))
	return blobRef
}

// mustParseHash is a test helper that parses a blob ref string (e.g. "blake3:hex")
// and returns the underlying Hash. Fails the test on error.
func mustParseHash(t *testing.T, hash string) contentcache.Hash {
	t.Helper()
	ref, err := contentcache.ParseBlobRef(hash)
	require.NoError(t, err)
	return ref.Hash
}

func TestAdmitNewBlobGoesToSmall(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{})
	hash := putBlob(t, ctx, mdb, b, "hello world")

	mgr.Admit(ctx, hash, int64(len("hello world")))

	n, err := mgr.queues.Len(QueueSmall)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	mgr.mu.Lock()
	require.Equal(t, int64(len("hello world")), mgr.smallBytes)
	mgr.mu.Unlock()
}

func TestNewManagerRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{
			name: "negative max size",
			cfg:  Config{MaxSize: -1},
		},
		{
			name: "small queue percent above 100",
			cfg:  Config{MaxSize: 100, SmallQueuePercent: 101},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			mdb := metadb.NewBoltDB(metadb.WithNoSync(true))
			require.NoError(t, mdb.Open(dir+"/meta.db"))
			t.Cleanup(func() { mdb.Close() })

			fsBackend, err := backend.NewFilesystem(dir + "/blobs")
			require.NoError(t, err)

			boltQueues, err := NewBoltQueues(mdb.DB())
			require.NoError(t, err)

			_, err = NewManager(boltQueues, mdb, fsBackend, tc.cfg)
			require.Error(t, err)
		})
	}
}

func TestAdmitGhostHitGoesToMain(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{})
	hash := putBlob(t, ctx, mdb, b, "hello world")

	// Simulate: hash was previously evicted to ghost.
	require.NoError(t, mgr.queues.GhostAdd(hash))

	// Re-admission after ghost hit → goes to main.
	mgr.Admit(ctx, hash, int64(len("hello world")))

	nSmall, _ := mgr.queues.Len(QueueSmall)
	nMain, _ := mgr.queues.Len(QueueMain)
	require.Equal(t, 0, nSmall)
	require.Equal(t, 1, nMain)

	ghostFound, _ := mgr.queues.GhostContains(hash)
	require.False(t, ghostFound, "ghost hit should clear ghost entry")
}

func TestEvictOneHitWonder(t *testing.T) {
	ctx := context.Background()
	// MaxSize = 5 bytes, blob is 10 bytes → cache over limit immediately.
	mgr, mdb, b := testManager(t, Config{MaxSize: 5})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	// AccessCount is 0 (one-hit wonder).
	mgr.Admit(ctx, hash, size)
	mgr.maybeEvict(ctx)

	// Should be evicted from small and added to ghost.
	nSmall, _ := mgr.queues.Len(QueueSmall)
	require.Equal(t, 0, nSmall, "blob should be evicted from small")

	ghostFound, _ := mgr.queues.GhostContains(hash)
	require.True(t, ghostFound, "evicted blob should appear in ghost")

	exists, _ := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)))
	require.False(t, exists, "blob should be deleted from backend")

	mgr.mu.Lock()
	require.Equal(t, int64(0), mgr.smallBytes)
	mgr.mu.Unlock()
}

func TestEvictPromotionToMain(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 5})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	// Simulate a cache hit: AccessCount > 0.
	entry, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	entry.AccessCount = 2
	require.NoError(t, mdb.PutBlob(ctx, entry))

	mgr.Admit(ctx, hash, size)
	mgr.maybeEvict(ctx)

	// AccessCount>0 → promoted to main, not evicted.
	nSmall, _ := mgr.queues.Len(QueueSmall)
	nMain, _ := mgr.queues.Len(QueueMain)
	require.Equal(t, 0, nSmall)
	require.Equal(t, 1, nMain)

	exists, _ := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)))
	require.True(t, exists, "promoted blob must not be deleted")

	// AccessCount is carried forward to main (per S3-FIFO paper).
	entry2, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, 2, entry2.AccessCount, "access count should be preserved on promotion")

	mgr.mu.Lock()
	require.Equal(t, int64(0), mgr.smallBytes)
	require.Equal(t, size, mgr.mainBytes)
	mgr.mu.Unlock()
}

func TestEvictMainSecondChance(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 5, SmallQueuePercent: 10})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	// Directly admit to main to bypass small queue logic.
	require.NoError(t, mgr.queues.PushHead(QueueMain, hash))
	mgr.mu.Lock()
	mgr.mainBytes = size
	mgr.mainLen = 1
	mgr.mu.Unlock()

	// AccessCount > 0 → second chance.
	entry, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	entry.AccessCount = 1
	require.NoError(t, mdb.PutBlob(ctx, entry))

	mgr.maybeEvict(ctx)

	// Blob gets second chance: decremented and reinserted, NOT evicted.
	nMain, _ := mgr.queues.Len(QueueMain)
	require.Equal(t, 1, nMain, "second-chance blob should remain in main")

	exists, _ := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)))
	require.True(t, exists)

	entry2, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, 0, entry2.AccessCount, "access count should be decremented to 0")
}

func TestEvictMainCold(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 5, SmallQueuePercent: 10})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	require.NoError(t, mgr.queues.PushHead(QueueMain, hash))
	mgr.mu.Lock()
	mgr.mainBytes = size
	mgr.mainLen = 1
	mgr.mu.Unlock()

	// AccessCount remains 0 (cold blob).
	mgr.maybeEvict(ctx)

	nMain, _ := mgr.queues.Len(QueueMain)
	require.Equal(t, 0, nMain, "cold blob should be evicted from main")

	exists, _ := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)))
	require.False(t, exists)

	// Main-queue evictions must NOT add to the ghost set — only small-queue
	// evictions do, to filter one-hit-wonders.
	ghostFound, _ := mgr.queues.GhostContains(hash)
	require.False(t, ghostFound, "main queue eviction must not add to ghost set")
}

func TestPinnedBlobSkipped(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 5})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	// Mark blob as referenced (pinned).
	entry, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	entry.RefCount = 1
	require.NoError(t, mdb.PutBlob(ctx, entry))

	mgr.Admit(ctx, hash, size)
	mgr.maybeEvict(ctx)

	// Pinned blob must not be evicted.
	nSmall, _ := mgr.queues.Len(QueueSmall)
	require.Equal(t, 1, nSmall, "pinned blob must remain in small queue")

	exists, _ := b.Exists(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)))
	require.True(t, exists)
}

func TestPinnedOverlimitDoesNotImmediatelyReschedule(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 5})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	entry, err := mdb.GetBlob(ctx, hash)
	require.NoError(t, err)
	entry.RefCount = 1
	require.NoError(t, mdb.PutBlob(ctx, entry))

	mgr.Admit(ctx, hash, size)

	// Admit signals once because the cache is over the size limit. Drain that
	// signal so this test only observes whether maybeEvict requeues itself.
	select {
	case <-mgr.evictCh:
	default:
		require.Fail(t, "expected admission to signal eviction")
	}

	mgr.maybeEvict(ctx)

	select {
	case <-mgr.evictCh:
		require.Fail(t, "pinned-only overlimit cache should wait for ticker or a later admission, not hot-loop")
	default:
	}
}

func TestManagerRemove(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{})
	hash := putBlob(t, ctx, mdb, b, "data")
	size := int64(4)

	mgr.Admit(ctx, hash, size)

	mgr.mu.Lock()
	require.Equal(t, size, mgr.smallBytes)
	mgr.mu.Unlock()

	mgr.Remove(ctx, hash, size)

	mgr.mu.Lock()
	require.Equal(t, int64(0), mgr.smallBytes)
	mgr.mu.Unlock()

	nSmall, _ := mgr.queues.Len(QueueSmall)
	require.Equal(t, 0, nSmall)
}

func TestRecomputeBytes(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{})

	h1 := putBlob(t, ctx, mdb, b, "blob1")
	h2 := putBlob(t, ctx, mdb, b, "blob2")
	mgr.Admit(ctx, h1, 5)
	mgr.Admit(ctx, h2, 5)

	// Simulate restart: reset in-memory counters and recompute from bbolt state.
	mgr.mu.Lock()
	mgr.smallBytes = 0
	mgr.mainBytes = 0
	mgr.smallLen = 0
	mgr.mainLen = 0
	mgr.ghostLen = 0
	mgr.mu.Unlock()

	require.NoError(t, mgr.recomputeState(ctx))

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	require.Equal(t, int64(10), mgr.smallBytes)
	require.Equal(t, int64(0), mgr.mainBytes)
}

func TestOrphanedQueueEntryCleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("small queue orphan is silently dropped", func(t *testing.T) {
		mgr, _, _ := testManager(t, Config{MaxSize: 5})

		// A hash that exists in the queue but has no MetaDB entry.
		orphanHash := strings.Repeat("a", 64)
		require.NoError(t, mgr.queues.PushHead(QueueSmall, orphanHash))
		mgr.mu.Lock()
		mgr.smallBytes = 10 // force over-limit so maybeEvict acts
		mgr.smallLen = 1
		mgr.mu.Unlock()

		mgr.maybeEvict(ctx)

		nSmall, _ := mgr.queues.Len(QueueSmall)
		require.Equal(t, 0, nSmall, "orphaned small queue entry should be silently dropped")
	})

	t.Run("main queue orphan is silently dropped", func(t *testing.T) {
		mgr, _, _ := testManager(t, Config{MaxSize: 5, SmallQueuePercent: 10})

		orphanHash := strings.Repeat("b", 64)
		require.NoError(t, mgr.queues.PushHead(QueueMain, orphanHash))
		mgr.mu.Lock()
		mgr.mainBytes = 10
		mgr.mainLen = 1
		mgr.mu.Unlock()

		mgr.maybeEvict(ctx)

		nMain, _ := mgr.queues.Len(QueueMain)
		require.Equal(t, 0, nMain, "orphaned main queue entry should be silently dropped")
	})
}

func TestConcurrentAdmitRemove(t *testing.T) {
	ctx := context.Background()
	mgr, mdb, b := testManager(t, Config{MaxSize: 10 * 1024 * 1024})

	const n = 50
	hashes := make([]string, n)
	sizes := make([]int64, n)
	for i := range hashes {
		content := fmt.Sprintf("blob-content-%d", i)
		hashes[i] = putBlob(t, ctx, mdb, b, content)
		sizes[i] = int64(len(content))
	}

	var wg sync.WaitGroup
	for i := range hashes {
		wg.Add(2)
		hash, size := hashes[i], sizes[i]
		go func() {
			defer wg.Done()
			mgr.Admit(ctx, hash, size)
		}()
		go func() {
			defer wg.Done()
			mgr.Remove(ctx, hash, size)
		}()
	}
	wg.Wait()

	// Byte counters must remain non-negative regardless of interleaving.
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	require.GreaterOrEqual(t, mgr.smallBytes, int64(0))
	require.GreaterOrEqual(t, mgr.mainBytes, int64(0))
}

func TestGhostHitEndToEnd(t *testing.T) {
	ctx := context.Background()
	// Tiny cache: 5 bytes, blob is 10 → will evict on first admission.
	mgr, mdb, b := testManager(t, Config{MaxSize: 5})
	content := "0123456789"
	hash := putBlob(t, ctx, mdb, b, content)
	size := int64(len(content))

	// First admission (no ghost): goes to small, evicted as one-hit wonder.
	mgr.Admit(ctx, hash, size)
	mgr.maybeEvict(ctx)

	ghostFound, _ := mgr.queues.GhostContains(hash)
	require.True(t, ghostFound)

	// Second admission (ghost hit): goes to main.
	// Re-write the blob to backend/metadb since it was deleted.
	require.NoError(t, b.Write(ctx, contentcache.BlobStorageKey(mustParseHash(t, hash)), strings.NewReader(content)))
	entry := &metadb.BlobEntry{Hash: hash, Size: size, CachedAt: time.Now(), LastAccess: time.Now()}
	require.NoError(t, mdb.PutBlob(ctx, entry))

	mgr.Admit(ctx, hash, size)

	nMain, _ := mgr.queues.Len(QueueMain)
	require.Equal(t, 1, nMain, "ghost hit should admit to main")

	ghostAfter, _ := mgr.queues.GhostContains(hash)
	require.False(t, ghostAfter, "ghost entry should be consumed")
}
