package gc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRawHex pads a short seed to a 64-char hex string (no algorithm prefix).
func testRawHex(seed string) string {
	h := seed
	for len(h) < 64 {
		h += "0"
	}
	return h[:64]
}

// testHash creates a "blake3:" prefixed hash string from a short seed
// suitable for storing in MetaDB (which requires the algorithm prefix).
func testHash(seed string) string {
	return "blake3:" + testRawHex(seed)
}

// testBlobKey returns the backend storage key for a test hash seed.
func testBlobKey(seed string) string {
	parsed, err := contentcache.ParseHash(testRawHex(seed))
	if err != nil {
		panic("invalid test hash: " + err.Error())
	}
	return contentcache.BlobStorageKey(parsed)
}

func newTestDB(t *testing.T, opts ...metadb.BoltDBOption) *metadb.BoltDB {
	t.Helper()
	db := metadb.NewBoltDB(opts...)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, db.Open(dbPath))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestBackend(t *testing.T) *backend.Filesystem {
	t.Helper()
	fs, err := backend.NewFilesystem(t.TempDir())
	require.NoError(t, err)
	return fs
}

func TestManager_RunNow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pastTime := now.Add(-time.Hour)
	db := newTestDB(t, metadb.WithNow(func() time.Time { return pastTime }))
	fs := newTestBackend(t)

	require.NoError(t, db.PutMeta(ctx, "npm", "expired-pkg", []byte(`{"name":"expired"}`), 10*time.Minute))

	db2 := newTestDB(t, metadb.WithNow(func() time.Time { return now }))
	require.NoError(t, db2.PutMeta(ctx, "npm", "valid-pkg", []byte(`{"name":"valid"}`), 24*time.Hour))

	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       testHash("aa"),
		Size:       100,
		CachedAt:   pastTime,
		LastAccess: pastTime,
		RefCount:   0,
	}))

	config := DefaultConfig()
	config.BatchSize = 100

	mgr := New(db, fs, config, nil, nil)
	result, err := mgr.RunNow(ctx)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.ExpiredMetaDeleted, "should delete 1 expired metadata")
	assert.Equal(t, 1, result.UnreferencedBlobsDeleted, "should delete 1 unreferenced blob")
	assert.Greater(t, result.Duration, time.Duration(0))
}

func TestManager_StartStop(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	fs := newTestBackend(t)

	config := DefaultConfig()
	config.StartupDelay = 10 * time.Millisecond
	config.Interval = 50 * time.Millisecond

	mgr := New(db, fs, config, nil, nil)
	mgr.Start(ctx)

	time.Sleep(100 * time.Millisecond)

	status := mgr.Status()
	require.NotNil(t, status, "should have run at least once")

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	err := mgr.Stop(stopCtx)
	require.NoError(t, err)

	err = mgr.Stop(stopCtx)
	require.NoError(t, err, "stop should be idempotent")
}

func TestManager_PhaseExpireMeta(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pastTime := now.Add(-time.Hour)
	db := newTestDB(t, metadb.WithNow(func() time.Time { return pastTime }))
	fs := newTestBackend(t)

	require.NoError(t, db.PutMeta(ctx, "npm", "expired1", []byte("data1"), 10*time.Minute))
	require.NoError(t, db.PutMeta(ctx, "npm", "expired2", []byte("data2"), 20*time.Minute))
	require.NoError(t, db.PutMeta(ctx, "pypi", "valid", []byte("data3"), 90*time.Minute))

	config := DefaultConfig()
	config.BatchSize = 100

	mgr := New(db, fs, config, nil, nil)
	result, err := mgr.RunNow(ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, result.ExpiredMetaDeleted)

	_, err = db.GetMeta(ctx, "npm", "expired1")
	require.ErrorIs(t, err, metadb.ErrNotFound)

	_, err = db.GetMeta(ctx, "npm", "expired2")
	require.ErrorIs(t, err, metadb.ErrNotFound)

	_, err = db.GetMeta(ctx, "pypi", "valid")
	require.NoError(t, err)
}

func TestManager_PhaseDeleteUnreferenced(t *testing.T) {
	ctx := context.Background()
	baseTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	db := newTestDB(t, metadb.WithNow(func() time.Time { return baseTime }))
	fs := newTestBackend(t)

	hash1 := testHash("aa01")
	hash2 := testHash("bb02")
	hash3 := testHash("cc03")

	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       hash1,
		Size:       100,
		CachedAt:   baseTime,
		LastAccess: baseTime,
		RefCount:   0,
	}))

	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       hash2,
		Size:       200,
		CachedAt:   baseTime,
		LastAccess: baseTime,
		RefCount:   0,
	}))

	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       hash3,
		Size:       300,
		CachedAt:   baseTime,
		LastAccess: baseTime,
		RefCount:   1,
	}))

	key1 := testBlobKey("aa01")
	key2 := testBlobKey("bb02")
	key3 := testBlobKey("cc03")
	require.NoError(t, fs.Write(ctx, key1, strings.NewReader("data1")))
	require.NoError(t, fs.Write(ctx, key2, strings.NewReader("data2")))
	require.NoError(t, fs.Write(ctx, key3, strings.NewReader("data3")))

	config := DefaultConfig()
	config.BatchSize = 100

	mgr := New(db, fs, config, nil, nil)
	result, err := mgr.RunNow(ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, result.UnreferencedBlobsDeleted)
	assert.Equal(t, int64(300), result.BytesReclaimed)

	_, err = db.GetBlob(ctx, hash1)
	require.ErrorIs(t, err, metadb.ErrNotFound)

	_, err = db.GetBlob(ctx, hash2)
	require.ErrorIs(t, err, metadb.ErrNotFound)

	_, err = db.GetBlob(ctx, hash3)
	require.NoError(t, err)

	exists, err := fs.Exists(ctx, key1)
	require.NoError(t, err)
	assert.False(t, exists)

	exists, err = fs.Exists(ctx, key3)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestManager_Status(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	fs := newTestBackend(t)

	config := DefaultConfig()

	mgr := New(db, fs, config, nil, nil)

	assert.Nil(t, mgr.Status(), "status should be nil before first run")

	result, err := mgr.RunNow(ctx)
	require.NoError(t, err)

	status := mgr.Status()
	require.NotNil(t, status)
	assert.Equal(t, result.StartedAt, status.StartedAt)
	assert.Equal(t, result.Duration, status.Duration)
	assert.Equal(t, result.UnreferencedBlobsDeleted, status.UnreferencedBlobsDeleted)
	assert.Equal(t, result.OrphanBlobsDeleted, status.OrphanBlobsDeleted)
	assert.Equal(t, result.ExpiredMetaDeleted, status.ExpiredMetaDeleted)
	assert.Equal(t, result.BytesReclaimed, status.BytesReclaimed)
}

func TestManager_PhaseDeleteOrphans(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	fs := newTestBackend(t)

	hash2 := testHash("bb00112233445566778899aabbccddeeff")
	key1 := testBlobKey("aa00112233445566778899aabbccddeeff")
	key2 := testBlobKey("bb00112233445566778899aabbccddeeff")
	require.NoError(t, fs.Write(ctx, key1, strings.NewReader("orphan data")))
	require.NoError(t, fs.Write(ctx, key2, strings.NewReader("tracked data")))

	now := time.Now()
	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       hash2,
		Size:       12,
		CachedAt:   now,
		LastAccess: now,
		RefCount:   1,
	}))

	config := DefaultConfig()
	config.BatchSize = 100

	mgr := New(db, fs, config, nil, nil)
	result, err := mgr.RunNow(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, result.OrphanBlobsDeleted)

	exists, err := fs.Exists(ctx, key1)
	require.NoError(t, err)
	assert.False(t, exists, "orphan blob should be deleted")

	exists, err = fs.Exists(ctx, key2)
	require.NoError(t, err)
	assert.True(t, exists, "tracked blob should remain")
}

func TestManager_ContextCancellation(t *testing.T) {
	db := newTestDB(t)
	fs := newTestBackend(t)

	config := DefaultConfig()
	config.StartupDelay = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	mgr := New(db, fs, config, nil, nil)
	mgr.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	cancel()

	time.Sleep(100 * time.Millisecond)
}

func TestManager_DoubleStart(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	fs := newTestBackend(t)

	config := DefaultConfig()
	config.StartupDelay = time.Hour

	mgr := New(db, fs, config, nil, nil)
	mgr.Start(ctx)
	mgr.Start(ctx)

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	err := mgr.Stop(stopCtx)
	require.NoError(t, err)
}

func TestParseBlobStorageKey(t *testing.T) {
	rawHex := testRawHex("ab01")

	tests := []struct {
		name    string
		key     string
		wantHex string
		wantErr bool
	}{
		{"valid 2-level key", "blobs/ab/" + rawHex, rawHex, false},
		{"valid 3-level key (legacy)", "blobs/ab/01/" + rawHex, rawHex, false},
		{"empty string", "", "", true},
		{"too short hash", "blobs/ab/abc123", "", true},
		{"non-hex chars", "blobs/ab/tempfile.tmp.000000000000000000000000000000000", "", true},
		{"temp file", "blobs/.tmp-upload-123", "", true},
		{"wrong prefix", "meta/ab/" + rawHex, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := contentcache.ParseBlobStorageKey(tt.key)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantHex, h.String())
			}
		})
	}
}
