package oci

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	tmpDir := t.TempDir()

	// Create and open BoltDB for envelope storage
	db := metadb.New()
	err := db.Open(filepath.Join(tmpDir, "metadata.db"))
	require.NoError(t, err)

	boltDB, ok := db.(*metadb.BoltDB)
	require.True(t, ok, "metaDB must be *metadb.BoltDB")

	// Create EnvelopeIndex instances for OCI
	imageIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "image", 24*time.Hour)
	require.NoError(t, err)
	manifestIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "manifest", 24*time.Hour)
	require.NoError(t, err)
	blobIndex, err := metadb.NewEnvelopeIndex(boltDB, "oci", "blob", 24*time.Hour)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return NewIndex(imageIndex, manifestIndex, blobIndex)
}

func TestIndexTagDigest(t *testing.T) {
	idx := newTestIndex(t)

	ctx := context.Background()

	t.Run("get non-existent tag", func(t *testing.T) {
		_, _, err := idx.GetTagDigest(ctx, "library/alpine", "latest")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("set and get tag", func(t *testing.T) {
		digest := "sha256:abc123def456"
		err := idx.SetTagDigest(ctx, "library/alpine", "latest", digest)
		require.NoError(t, err)

		gotDigest, refreshedAt, err := idx.GetTagDigest(ctx, "library/alpine", "latest")
		require.NoError(t, err)
		require.Equal(t, digest, gotDigest)
		require.False(t, refreshedAt.IsZero())
	})

	t.Run("update tag", func(t *testing.T) {
		newDigest := "sha256:newdigest789"
		err := idx.SetTagDigest(ctx, "library/alpine", "latest", newDigest)
		require.NoError(t, err)

		gotDigest, _, err := idx.GetTagDigest(ctx, "library/alpine", "latest")
		require.NoError(t, err)
		require.Equal(t, newDigest, gotDigest)
	})

	t.Run("multiple tags", func(t *testing.T) {
		_ = idx.SetTagDigest(ctx, "library/alpine", "3.18", "sha256:version318")
		_ = idx.SetTagDigest(ctx, "library/alpine", "3.19", "sha256:version319")

		d1, _, _ := idx.GetTagDigest(ctx, "library/alpine", "3.18")
		d2, _, _ := idx.GetTagDigest(ctx, "library/alpine", "3.19")

		require.Equal(t, "sha256:version318", d1)
		require.Equal(t, "sha256:version319", d2)
	})
}

func TestIndexRefreshTag(t *testing.T) {
	idx := newTestIndex(t)

	ctx := context.Background()

	// Set up a tag
	_ = idx.SetTagDigest(ctx, "library/nginx", "latest", "sha256:original")
	_, originalRefresh, _ := idx.GetTagDigest(ctx, "library/nginx", "latest")

	// Wait a bit and refresh
	time.Sleep(10 * time.Millisecond)
	err := idx.RefreshTag(ctx, "library/nginx", "latest")
	require.NoError(t, err)

	_, newRefresh, _ := idx.GetTagDigest(ctx, "library/nginx", "latest")
	require.True(t, newRefresh.After(originalRefresh))
}

func TestIndexManifest(t *testing.T) {
	idx := newTestIndex(t)

	ctx := context.Background()

	t.Run("get non-existent manifest", func(t *testing.T) {
		_, err := idx.GetManifest(ctx, "sha256:nonexistent")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("put and get manifest", func(t *testing.T) {
		digest := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		mediaType := "application/vnd.oci.image.manifest.v1+json"
		hash := contentcache.Hash{1, 2, 3, 4}
		size := int64(1024)

		err := idx.PutManifest(ctx, digest, mediaType, hash, size)
		require.NoError(t, err)

		manifest, err := idx.GetManifest(ctx, digest)
		require.NoError(t, err)
		require.Equal(t, digest, manifest.Digest)
		require.Equal(t, mediaType, manifest.MediaType)
		require.Equal(t, hash, manifest.ContentHash)
		require.Equal(t, size, manifest.Size)
	})
}

func TestIndexBlob(t *testing.T) {
	idx := newTestIndex(t)

	ctx := context.Background()

	t.Run("get non-existent blob", func(t *testing.T) {
		_, err := idx.GetBlob(ctx, "sha256:nonexistent")
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("put and get blob", func(t *testing.T) {
		digest := "sha256:abc123def456789"
		hash := contentcache.Hash{5, 6, 7, 8}
		size := int64(2048)

		err := idx.PutBlob(ctx, digest, hash, size)
		require.NoError(t, err)

		blob, err := idx.GetBlob(ctx, digest)
		require.NoError(t, err)
		require.Equal(t, digest, blob.Digest)
		require.Equal(t, hash, blob.ContentHash)
		require.Equal(t, size, blob.Size)
	})
}

func TestIndexListImages(t *testing.T) {
	idx := newTestIndex(t)

	ctx := context.Background()

	// Empty list
	images, err := idx.ListImages(ctx)
	require.NoError(t, err)
	require.Empty(t, images)

	// Add some images
	_ = idx.SetTagDigest(ctx, "library/alpine", "latest", "sha256:a")
	_ = idx.SetTagDigest(ctx, "library/nginx", "latest", "sha256:b")
	_ = idx.SetTagDigest(ctx, "myrepo/myimage", "v1", "sha256:c")

	images, err = idx.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, images, 3)
}

func TestEncodeDecodeImageName(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"library/alpine"},
		{"myrepo/myimage"},
		{"gcr.io/project/image"},
		{"registry.example.com:5000/nested/path/image"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeImageName(tt.name)
			decoded, err := decodeImageName(encoded)
			require.NoError(t, err)
			require.Equal(t, tt.name, decoded)
		})
	}
}
