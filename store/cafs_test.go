package store

import (
	"bytes"
	"context"
	"io"
	"testing"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/stretchr/testify/require"
)

func TestCAFSPutGet(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("test content for CAFS")

	// Put
	hash, err := cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	require.False(t, hash.IsZero())

	// Verify hash is correct
	expectedHash := contentcache.HashBytes(data)
	require.Equal(t, expectedHash, hash)

	// Get
	rc, err := cafs.Get(ctx, hash)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestCAFSPutWithResult(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("test content")

	// First put
	result1, err := cafs.PutWithResult(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	require.False(t, result1.Exists)
	require.Equal(t, int64(len(data)), result1.Size)

	// Second put of same content
	result2, err := cafs.PutWithResult(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	require.True(t, result2.Exists)
	require.Equal(t, result1.Hash, result2.Hash)
}

func TestCAFSPutBytes(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("bytes convenience method")

	hash, err := cafs.PutBytes(ctx, data)
	require.NoError(t, err)

	// Verify via GetBytes
	got, err := cafs.GetBytes(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestCAFSGetNotFound(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()

	// Try to get non-existent hash
	fakeHash := contentcache.HashBytes([]byte("does not exist"))

	_, err := cafs.Get(ctx, fakeHash)
	require.ErrorIs(t, err, backend.ErrNotFound)
}

func TestCAFSHas(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("existence check")

	hash := contentcache.HashBytes(data)

	// Before put
	exists, err := cafs.Has(ctx, hash)
	require.NoError(t, err)
	require.False(t, exists)

	// Put
	_, err = cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	// After put
	exists, err = cafs.Has(ctx, hash)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestCAFSDelete(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("delete me")

	// Put
	hash, err := cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	// Delete
	err = cafs.Delete(ctx, hash)
	require.NoError(t, err)

	// Verify deleted
	exists, _ := cafs.Has(ctx, hash)
	require.False(t, exists)

	// Delete again should not error
	err = cafs.Delete(ctx, hash)
	require.NoError(t, err)
}

func TestCAFSSize(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("size check content")

	hash, err := cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	size, err := cafs.Size(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), size)
}

func TestCAFSSizeNotFound(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	fakeHash := contentcache.HashBytes([]byte("not stored"))

	_, err := cafs.Size(ctx, fakeHash)
	require.ErrorIs(t, err, backend.ErrNotFound)
}

func TestCAFSList(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()

	// Put multiple items
	data := [][]byte{
		[]byte("content one"),
		[]byte("content two"),
		[]byte("content three"),
	}

	expectedHashes := make(map[contentcache.Hash]bool)
	for _, d := range data {
		hash, err := cafs.Put(ctx, bytes.NewReader(d))
		require.NoError(t, err)
		expectedHashes[hash] = true
	}

	// List
	hashes, err := cafs.List(ctx)
	require.NoError(t, err)
	require.Len(t, hashes, len(data))

	for _, h := range hashes {
		require.True(t, expectedHashes[h])
	}
}

func TestCAFSDeduplication(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("deduplicated content")

	// Put same content multiple times
	hash1, _ := cafs.Put(ctx, bytes.NewReader(data))
	hash2, _ := cafs.Put(ctx, bytes.NewReader(data))
	hash3, _ := cafs.Put(ctx, bytes.NewReader(data))

	// All hashes should be the same
	require.Equal(t, hash1, hash2)
	require.Equal(t, hash2, hash3)

	// List should show only one item
	hashes, err := cafs.List(ctx)
	require.NoError(t, err)
	require.Len(t, hashes, 1)
}

func TestCAFSEmptyContent(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte{}

	hash, err := cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	got, err := cafs.GetBytes(ctx, hash)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCAFSLargeContent(t *testing.T) {
	cafs, cleanup := newTestCAFS(t)
	defer cleanup()

	ctx := context.Background()

	// 1MB of data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	hash, err := cafs.Put(ctx, bytes.NewReader(data))
	require.NoError(t, err)

	got, err := cafs.GetBytes(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

// Helper functions

func newTestCAFS(t *testing.T) (*CAFS, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	b, err := backend.NewFilesystem(tmpDir)
	require.NoError(t, err)
	return NewCAFS(b), func() {}
}
