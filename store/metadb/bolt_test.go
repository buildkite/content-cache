package metadb

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBoltDB(t *testing.T, opts ...BoltDBOption) *BoltDB {
	t.Helper()
	db := NewBoltDB(opts...)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	t.Logf("dbPath: %s", dbPath)
	require.NoError(t, db.Open(dbPath))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBoltDB_MetadataOperations(t *testing.T) {
	ctx := context.Background()

	t.Run("PutMeta and GetMeta round-trip", func(t *testing.T) {
		db := newTestBoltDB(t)

		protocol := "npm"
		key := "lodash@4.17.21"
		data := []byte(`{"name":"lodash","version":"4.17.21"}`)

		err := db.PutMeta(ctx, protocol, key, data, time.Hour)
		require.NoError(t, err)

		got, err := db.GetMeta(ctx, protocol, key)
		require.NoError(t, err)
		assert.Equal(t, data, got)
	})

	t.Run("GetMeta returns ErrNotFound for missing key", func(t *testing.T) {
		db := newTestBoltDB(t)

		_, err := db.GetMeta(ctx, "npm", "nonexistent")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("DeleteMeta removes entry", func(t *testing.T) {
		db := newTestBoltDB(t)

		protocol := "npm"
		key := "express@4.18.0"
		data := []byte(`{"name":"express"}`)

		require.NoError(t, db.PutMeta(ctx, protocol, key, data, time.Hour))

		err := db.DeleteMeta(ctx, protocol, key)
		require.NoError(t, err)

		_, err = db.GetMeta(ctx, protocol, key)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("ListMeta returns all keys for a protocol", func(t *testing.T) {
		db := newTestBoltDB(t)

		require.NoError(t, db.PutMeta(ctx, "npm", "pkg1", []byte("data1"), time.Hour))
		require.NoError(t, db.PutMeta(ctx, "npm", "pkg2", []byte("data2"), time.Hour))
		require.NoError(t, db.PutMeta(ctx, "npm", "pkg3", []byte("data3"), time.Hour))
		require.NoError(t, db.PutMeta(ctx, "pypi", "other", []byte("other"), time.Hour))

		keys, err := db.ListMeta(ctx, "npm")
		require.NoError(t, err)
		assert.Len(t, keys, 3)
		assert.ElementsMatch(t, []string{"pkg1", "pkg2", "pkg3"}, keys)
	})
}

func TestBoltDB_BlobOperations(t *testing.T) {
	ctx := context.Background()

	t.Run("PutBlob and GetBlob round-trip", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now().Truncate(time.Second)
		entry := &BlobEntry{
			Hash:       "abc123",
			Size:       1024,
			CachedAt:   now,
			LastAccess: now,
			RefCount:   1,
		}

		err := db.PutBlob(ctx, entry)
		require.NoError(t, err)

		got, err := db.GetBlob(ctx, "abc123")
		require.NoError(t, err)
		assert.Equal(t, entry.Hash, got.Hash)
		assert.Equal(t, entry.Size, got.Size)
		assert.Equal(t, entry.RefCount, got.RefCount)
	})

	t.Run("IncrementBlobRef increases RefCount", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		entry := &BlobEntry{Hash: "hash1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 1}
		require.NoError(t, db.PutBlob(ctx, entry))

		err := db.IncrementBlobRef(ctx, "hash1")
		require.NoError(t, err)

		got, err := db.GetBlob(ctx, "hash1")
		require.NoError(t, err)
		assert.Equal(t, 2, got.RefCount)
	})

	t.Run("DecrementBlobRef decreases RefCount", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		entry := &BlobEntry{Hash: "hash2", Size: 100, CachedAt: now, LastAccess: now, RefCount: 3}
		require.NoError(t, db.PutBlob(ctx, entry))

		err := db.DecrementBlobRef(ctx, "hash2")
		require.NoError(t, err)

		got, err := db.GetBlob(ctx, "hash2")
		require.NoError(t, err)
		assert.Equal(t, 2, got.RefCount)
	})

	t.Run("TouchBlob updates LastAccess", func(t *testing.T) {
		initialTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		updatedTime := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

		currentTime := initialTime
		db := newTestBoltDB(t, WithNow(func() time.Time { return currentTime }))

		entry := &BlobEntry{Hash: "hash3", Size: 100, CachedAt: initialTime, LastAccess: initialTime, RefCount: 1}
		require.NoError(t, db.PutBlob(ctx, entry))

		currentTime = updatedTime
		newCount, err := db.TouchBlob(ctx, "hash3")
		require.NoError(t, err)
		assert.Equal(t, 1, newCount)

		got, err := db.GetBlob(ctx, "hash3")
		require.NoError(t, err)
		assert.Equal(t, updatedTime, got.LastAccess)
	})

	t.Run("TouchBlob increments AccessCount and caps at 3", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		entry := &BlobEntry{Hash: "countblob", Size: 50, CachedAt: now, LastAccess: now}
		require.NoError(t, db.PutBlob(ctx, entry))

		// First four touches: count goes 0→1→2→3→3 (capped)
		for i := 1; i <= 4; i++ {
			newCount, err := db.TouchBlob(ctx, "countblob")
			require.NoError(t, err)
			expected := i
			if expected > 3 {
				expected = 3
			}
			assert.Equal(t, expected, newCount, "touch %d", i)
		}

		got, err := db.GetBlob(ctx, "countblob")
		require.NoError(t, err)
		assert.Equal(t, 3, got.AccessCount)
	})

	t.Run("TouchBlob on missing blob returns ErrNotFound", func(t *testing.T) {
		db := newTestBoltDB(t)

		_, err := db.TouchBlob(ctx, "nosuchblob")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("DeleteBlob removes entry", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		entry := &BlobEntry{Hash: "todelete", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}
		require.NoError(t, db.PutBlob(ctx, entry))

		err := db.DeleteBlob(ctx, "todelete")
		require.NoError(t, err)

		_, err = db.GetBlob(ctx, "todelete")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("TotalBlobSize returns sum of all blob sizes", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "a", Size: 100, CachedAt: now, LastAccess: now}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "b", Size: 200, CachedAt: now, LastAccess: now}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "c", Size: 300, CachedAt: now, LastAccess: now}))

		total, err := db.TotalBlobSize(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(600), total)
	})
}

func TestBoltDB_ExpiryQueries(t *testing.T) {
	ctx := context.Background()

	t.Run("GetExpiredMeta returns entries past expiry time", func(t *testing.T) {
		baseTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		db := newTestBoltDB(t, WithNow(func() time.Time { return baseTime }))

		require.NoError(t, db.PutMeta(ctx, "npm", "expired1", []byte("data"), 10*time.Minute))
		require.NoError(t, db.PutMeta(ctx, "npm", "expired2", []byte("data"), 20*time.Minute))
		require.NoError(t, db.PutMeta(ctx, "npm", "valid", []byte("data"), 2*time.Hour))

		checkTime := baseTime.Add(30 * time.Minute)
		expired, err := db.GetExpiredMeta(ctx, checkTime, 0)
		require.NoError(t, err)
		assert.Len(t, expired, 2)

		var keys []string
		for _, e := range expired {
			keys = append(keys, e.Key)
		}
		assert.ElementsMatch(t, []string{"expired1", "expired2"}, keys)
	})

	t.Run("GetExpiredMeta respects limit", func(t *testing.T) {
		baseTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		db := newTestBoltDB(t, WithNow(func() time.Time { return baseTime }))

		require.NoError(t, db.PutMeta(ctx, "npm", "e1", []byte("data"), 5*time.Minute))
		require.NoError(t, db.PutMeta(ctx, "npm", "e2", []byte("data"), 10*time.Minute))
		require.NoError(t, db.PutMeta(ctx, "npm", "e3", []byte("data"), 15*time.Minute))

		checkTime := baseTime.Add(time.Hour)
		expired, err := db.GetExpiredMeta(ctx, checkTime, 2)
		require.NoError(t, err)
		assert.Len(t, expired, 2)
	})

	t.Run("GetExpiredMeta returns empty for no expired entries", func(t *testing.T) {
		baseTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		db := newTestBoltDB(t, WithNow(func() time.Time { return baseTime }))

		require.NoError(t, db.PutMeta(ctx, "npm", "fresh", []byte("data"), 24*time.Hour))

		checkTime := baseTime.Add(time.Hour)
		expired, err := db.GetExpiredMeta(ctx, checkTime, 0)
		require.NoError(t, err)
		assert.Empty(t, expired)
	})
}

func TestBoltDB_UnreferencedBlobQueries(t *testing.T) {
	ctx := context.Background()

	t.Run("GetUnreferencedBlobs returns blobs with RefCount == 0", func(t *testing.T) {
		db := newTestBoltDB(t)

		now := time.Now()
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "ref0", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "ref1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 1}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "ref0-2", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))

		// Zero before = no retention filter, return all unreferenced.
		hashes, err := db.GetUnreferencedBlobs(ctx, time.Time{}, 0)
		require.NoError(t, err)
		assert.Len(t, hashes, 2)
		assert.ElementsMatch(t, []string{"ref0", "ref0-2"}, hashes)
	})

	t.Run("GetUnreferencedBlobs respects retention floor", func(t *testing.T) {
		db := newTestBoltDB(t)

		old := time.Now().Add(-48 * time.Hour)
		recent := time.Now().Add(-1 * time.Hour)

		// Old blob: last accessed 48h ago, eligible for deletion.
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "old", Size: 100, CachedAt: old, LastAccess: old, RefCount: 0}))
		// Recent blob: last accessed 1h ago, within 24h retention window.
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "recent", Size: 100, CachedAt: recent, LastAccess: recent, RefCount: 0}))
		// Referenced blob: should never appear regardless.
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "pinned", Size: 100, CachedAt: old, LastAccess: old, RefCount: 1}))

		// Cutoff = now - 24h: only blobs last seen before that are eligible.
		before := time.Now().Add(-24 * time.Hour)
		hashes, err := db.GetUnreferencedBlobs(ctx, before, 0)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"old"}, hashes)
	})
}

func TestBoltDB_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	db := newTestBoltDB(t) // Toggle: newTestBoltDB(t, WithNoSync(true)) to disable fsync

	const numGoroutines = 10
	const numOps = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				protocol := "npm"
				key := string(rune('a'+id)) + string(rune('0'+j%10)) //nolint:gosec // bounded: id < numGoroutines (10), j%10 < 10
				data := []byte("data")

				_ = db.PutMeta(ctx, protocol, key, data, time.Hour)

				_, _ = db.GetMeta(ctx, protocol, key)

				now := time.Now()
				hash := string(rune('h'+id)) + string(rune('0'+j%10)) //nolint:gosec // bounded: id < numGoroutines (10), j%10 < 10
				_ = db.PutBlob(ctx, &BlobEntry{Hash: hash, Size: 100, CachedAt: now, LastAccess: now, RefCount: 1})

				_, _ = db.GetBlob(ctx, hash)

				_, _ = db.TouchBlob(ctx, hash)
			}
		}(i)
	}

	wg.Wait()

	keys, err := db.ListMeta(ctx, "npm")
	require.NoError(t, err)
	assert.NotEmpty(t, keys)

	total, err := db.TotalBlobSize(ctx)
	require.NoError(t, err)
	assert.Positive(t, total)

	t.Logf("total: %v, keys: %v", total, len(keys))
}

func TestBoltDB_PutMetaWithRefs(t *testing.T) {
	ctx := context.Background()

	t.Run("increments refs for new key", func(t *testing.T) {
		db := newTestBoltDB(t)
		now := time.Now()

		// Create blobs first
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash2", Size: 200, CachedAt: now, LastAccess: now, RefCount: 0}))

		// Put meta with refs
		err := db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"name":"pkg1"}`), time.Hour, []string{"hash1", "hash2"})
		require.NoError(t, err)

		// Check refs were incremented
		blob1, err := db.GetBlob(ctx, "hash1")
		require.NoError(t, err)
		assert.Equal(t, 1, blob1.RefCount)

		blob2, err := db.GetBlob(ctx, "hash2")
		require.NoError(t, err)
		assert.Equal(t, 1, blob2.RefCount)

		// Check refs are stored
		refs, err := db.GetMetaBlobRefs(ctx, "npm", "pkg1")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"hash1", "hash2"}, refs)
	})

	t.Run("computes diff on overwrite", func(t *testing.T) {
		db := newTestBoltDB(t)
		now := time.Now()

		// Create blobs
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash2", Size: 200, CachedAt: now, LastAccess: now, RefCount: 0}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash3", Size: 300, CachedAt: now, LastAccess: now, RefCount: 0}))

		// First put: refs = [hash1, hash2]
		err := db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":1}`), time.Hour, []string{"hash1", "hash2"})
		require.NoError(t, err)

		// Second put: refs = [hash2, hash3] (hash1 removed, hash3 added)
		err = db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":2}`), time.Hour, []string{"hash2", "hash3"})
		require.NoError(t, err)

		// hash1: was 1, now 0 (decremented)
		blob1, err := db.GetBlob(ctx, "hash1")
		require.NoError(t, err)
		assert.Equal(t, 0, blob1.RefCount)

		// hash2: was 1, still 1 (no change)
		blob2, err := db.GetBlob(ctx, "hash2")
		require.NoError(t, err)
		assert.Equal(t, 1, blob2.RefCount)

		// hash3: was 0, now 1 (incremented)
		blob3, err := db.GetBlob(ctx, "hash3")
		require.NoError(t, err)
		assert.Equal(t, 1, blob3.RefCount)
	})

	t.Run("is idempotent with same refs", func(t *testing.T) {
		db := newTestBoltDB(t)
		now := time.Now()

		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))

		// Put twice with same refs
		err := db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":1}`), time.Hour, []string{"hash1"})
		require.NoError(t, err)

		err = db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":2}`), time.Hour, []string{"hash1"})
		require.NoError(t, err)

		// RefCount should still be 1 (not 2)
		blob1, err := db.GetBlob(ctx, "hash1")
		require.NoError(t, err)
		assert.Equal(t, 1, blob1.RefCount)
	})

	t.Run("handles refs to nonexistent blobs gracefully", func(t *testing.T) {
		db := newTestBoltDB(t)

		// Put meta with ref to blob that doesn't exist yet
		err := db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":1}`), time.Hour, []string{"future-hash"})
		require.NoError(t, err)

		// Refs are still tracked
		refs, err := db.GetMetaBlobRefs(ctx, "npm", "pkg1")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"future-hash"}, refs)
	})
}

func TestBoltDB_DeleteMetaWithRefs(t *testing.T) {
	ctx := context.Background()

	t.Run("decrements all refs on delete", func(t *testing.T) {
		db := newTestBoltDB(t)
		now := time.Now()

		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash1", Size: 100, CachedAt: now, LastAccess: now, RefCount: 0}))
		require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: "hash2", Size: 200, CachedAt: now, LastAccess: now, RefCount: 0}))

		// Put meta with refs
		err := db.PutMetaWithRefs(ctx, "npm", "pkg1", []byte(`{"v":1}`), time.Hour, []string{"hash1", "hash2"})
		require.NoError(t, err)

		// Delete meta
		err = db.DeleteMetaWithRefs(ctx, "npm", "pkg1")
		require.NoError(t, err)

		// Both refs should be decremented
		blob1, err := db.GetBlob(ctx, "hash1")
		require.NoError(t, err)
		assert.Equal(t, 0, blob1.RefCount)

		blob2, err := db.GetBlob(ctx, "hash2")
		require.NoError(t, err)
		assert.Equal(t, 0, blob2.RefCount)

		// Meta should be gone
		_, err = db.GetMeta(ctx, "npm", "pkg1")
		require.ErrorIs(t, err, ErrNotFound)

		// Refs should be gone
		refs, err := db.GetMetaBlobRefs(ctx, "npm", "pkg1")
		require.NoError(t, err)
		assert.Empty(t, refs)
	})
}

func TestBoltDB_UpdateJSON(t *testing.T) {
	ctx := context.Background()

	type Counter struct {
		Value int `json:"value"`
	}

	t.Run("read-modify-write is atomic", func(t *testing.T) {
		db := newTestBoltDB(t)

		// Initial put
		err := db.PutMeta(ctx, "test", "counter", []byte(`{"value":0}`), time.Hour)
		require.NoError(t, err)

		// Update atomically
		var counter Counter
		err = db.UpdateJSON(ctx, "test", "counter", time.Hour, func(v any) error {
			c := v.(*Counter)
			c.Value++
			return nil
		}, &counter)
		require.NoError(t, err)
		assert.Equal(t, 1, counter.Value)

		// Verify persisted
		var got Counter
		data, err := db.GetMeta(ctx, "test", "counter")
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, 1, got.Value)
	})

	t.Run("creates new entry if not found", func(t *testing.T) {
		db := newTestBoltDB(t)

		var counter Counter
		err := db.UpdateJSON(ctx, "test", "new-counter", time.Hour, func(v any) error {
			c := v.(*Counter)
			c.Value = 42
			return nil
		}, &counter)
		require.NoError(t, err)
		assert.Equal(t, 42, counter.Value)

		// Verify persisted
		data, err := db.GetMeta(ctx, "test", "new-counter")
		require.NoError(t, err)
		var got Counter
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, 42, got.Value)
	})

	t.Run("concurrent updates don't lose data", func(t *testing.T) {
		db := newTestBoltDB(t)

		// Initialize counter
		err := db.PutMeta(ctx, "test", "counter", []byte(`{"value":0}`), time.Hour)
		require.NoError(t, err)

		const numGoroutines = 10
		const numOps = 20

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < numOps; j++ {
					var counter Counter
					_ = db.UpdateJSON(ctx, "test", "counter", time.Hour, func(v any) error {
						c := v.(*Counter)
						c.Value++
						return nil
					}, &counter)
				}
			}()
		}

		wg.Wait()

		// Should have exactly numGoroutines * numOps increments
		data, err := db.GetMeta(ctx, "test", "counter")
		require.NoError(t, err)
		var final Counter
		require.NoError(t, json.Unmarshal(data, &final))
		assert.Equal(t, numGoroutines*numOps, final.Value)
	})
}

func TestDiffRefs(t *testing.T) {
	tests := []struct {
		name        string
		oldRefs     []string
		newRefs     []string
		wantAdded   []string
		wantRemoved []string
	}{
		{
			name:        "empty to some",
			oldRefs:     nil,
			newRefs:     []string{"a", "b"},
			wantAdded:   []string{"a", "b"},
			wantRemoved: nil,
		},
		{
			name:        "some to empty",
			oldRefs:     []string{"a", "b"},
			newRefs:     nil,
			wantAdded:   nil,
			wantRemoved: []string{"a", "b"},
		},
		{
			name:        "partial overlap",
			oldRefs:     []string{"a", "b"},
			newRefs:     []string{"b", "c"},
			wantAdded:   []string{"c"},
			wantRemoved: []string{"a"},
		},
		{
			name:        "same refs",
			oldRefs:     []string{"a", "b"},
			newRefs:     []string{"a", "b"},
			wantAdded:   nil,
			wantRemoved: nil,
		},
		{
			name:        "complete replacement",
			oldRefs:     []string{"a", "b"},
			newRefs:     []string{"c", "d"},
			wantAdded:   []string{"c", "d"},
			wantRemoved: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := diffRefs(tt.oldRefs, tt.newRefs)
			assert.ElementsMatch(t, tt.wantAdded, added)
			assert.ElementsMatch(t, tt.wantRemoved, removed)
		})
	}
}
