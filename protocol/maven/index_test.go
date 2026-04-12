package maven

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func newTestIndex(t *testing.T) (*Index, *metadb.BoltDB) {
	t.Helper()
	tmpDir := t.TempDir()

	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	require.NoError(t, db.Open(filepath.Join(tmpDir, "test.db")))
	t.Cleanup(func() { _ = db.Close() })

	metadataIdx, err := metadb.NewEnvelopeIndex(db, "maven", kindMetadata, 5*time.Minute)
	require.NoError(t, err)
	artifactIdx, err := metadb.NewEnvelopeIndex(db, "maven", kindArtifact, 24*time.Hour)
	require.NoError(t, err)

	return NewIndex(metadataIdx, artifactIdx), db
}

func TestIndexCachedMetadata(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()
	groupID := "org.apache.commons"
	artifactID := "commons-lang3"

	// Get should fail initially
	_, err := idx.GetCachedMetadata(ctx, groupID, artifactID)
	require.ErrorIs(t, err, ErrNotFound)

	// Put metadata
	meta := &CachedMetadata{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Metadata:   []byte(`<?xml version="1.0"?><metadata></metadata>`),
	}
	err = idx.PutCachedMetadata(ctx, meta)
	require.NoError(t, err)

	// Get should succeed
	got, err := idx.GetCachedMetadata(ctx, groupID, artifactID)
	require.NoError(t, err)
	require.Equal(t, groupID, got.GroupID)
	require.Equal(t, artifactID, got.ArtifactID)
	require.Contains(t, string(got.Metadata), "<metadata>")
	require.False(t, got.CachedAt.IsZero())
	require.False(t, got.UpdatedAt.IsZero())
}

func TestIndexCachedArtifact(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()
	coord := ArtifactCoordinate{
		GroupID:    "org.apache.commons",
		ArtifactID: "commons-lang3",
		Version:    "3.12.0",
		Extension:  "jar",
	}

	// Get should fail initially
	_, err := idx.GetCachedArtifact(ctx, coord)
	require.ErrorIs(t, err, ErrNotFound)

	// Put artifact
	artifact := &CachedArtifact{
		GroupID:    coord.GroupID,
		ArtifactID: coord.ArtifactID,
		Version:    coord.Version,
		Extension:  coord.Extension,
		Hash:       contentcache.HashBytes([]byte("test content")),
		Size:       1024,
		Checksums: Checksums{
			SHA1: "abc123",
			MD5:  "def456",
		},
	}
	err = idx.PutCachedArtifact(ctx, artifact)
	require.NoError(t, err)

	// Get should succeed
	got, err := idx.GetCachedArtifact(ctx, coord)
	require.NoError(t, err)
	require.Equal(t, coord.GroupID, got.GroupID)
	require.Equal(t, coord.ArtifactID, got.ArtifactID)
	require.Equal(t, coord.Version, got.Version)
	require.Equal(t, int64(1024), got.Size)
	require.Equal(t, "abc123", got.Checksums.SHA1)
	require.False(t, got.CachedAt.IsZero())
}

func TestIndexCachedArtifactWithClassifier(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()

	// Store artifact without classifier
	coord1 := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "test",
		Version:    "1.0.0",
		Extension:  "jar",
	}
	artifact1 := &CachedArtifact{
		GroupID:    coord1.GroupID,
		ArtifactID: coord1.ArtifactID,
		Version:    coord1.Version,
		Extension:  coord1.Extension,
		Hash:       contentcache.HashBytes([]byte("main jar")),
		Size:       100,
	}
	err := idx.PutCachedArtifact(ctx, artifact1)
	require.NoError(t, err)

	// Store artifact with sources classifier
	coord2 := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "test",
		Version:    "1.0.0",
		Classifier: "sources",
		Extension:  "jar",
	}
	artifact2 := &CachedArtifact{
		GroupID:    coord2.GroupID,
		ArtifactID: coord2.ArtifactID,
		Version:    coord2.Version,
		Classifier: coord2.Classifier,
		Extension:  coord2.Extension,
		Hash:       contentcache.HashBytes([]byte("sources jar")),
		Size:       200,
	}
	err = idx.PutCachedArtifact(ctx, artifact2)
	require.NoError(t, err)

	// Both should be retrievable separately
	got1, err := idx.GetCachedArtifact(ctx, coord1)
	require.NoError(t, err)
	require.Equal(t, int64(100), got1.Size)

	got2, err := idx.GetCachedArtifact(ctx, coord2)
	require.NoError(t, err)
	require.Equal(t, int64(200), got2.Size)
	require.Equal(t, "sources", got2.Classifier)
}

func TestIndexGetArtifactHash(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()
	coord := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "test",
		Version:    "1.0.0",
		Extension:  "jar",
	}

	// Get should fail initially
	_, err := idx.GetArtifactHash(ctx, coord)
	require.ErrorIs(t, err, ErrNotFound)

	// Store artifact
	expectedHash := contentcache.HashBytes([]byte("test content"))
	artifact := &CachedArtifact{
		GroupID:    coord.GroupID,
		ArtifactID: coord.ArtifactID,
		Version:    coord.Version,
		Extension:  coord.Extension,
		Hash:       expectedHash,
		Size:       100,
	}
	err = idx.PutCachedArtifact(ctx, artifact)
	require.NoError(t, err)

	// Get hash should succeed
	gotHash, err := idx.GetArtifactHash(ctx, coord)
	require.NoError(t, err)
	require.Equal(t, expectedHash, gotHash)
}

func TestIndexDeleteMetadata(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()
	groupID := "org.example"
	artifactID := "delete-me"

	// Put metadata
	err := idx.PutCachedMetadata(ctx, &CachedMetadata{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Metadata:   []byte(`<?xml version="1.0"?>`),
	})
	require.NoError(t, err)

	// Delete
	err = idx.DeleteMetadata(ctx, groupID, artifactID)
	require.NoError(t, err)

	// Get should fail
	_, err = idx.GetCachedMetadata(ctx, groupID, artifactID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestIndexDeleteArtifact(t *testing.T) {
	idx, _ := newTestIndex(t)

	ctx := context.Background()
	coord := ArtifactCoordinate{
		GroupID:    "org.example",
		ArtifactID: "delete-me",
		Version:    "1.0.0",
		Extension:  "jar",
	}

	// Put artifact
	err := idx.PutCachedArtifact(ctx, &CachedArtifact{
		GroupID:    coord.GroupID,
		ArtifactID: coord.ArtifactID,
		Version:    coord.Version,
		Extension:  coord.Extension,
		Hash:       contentcache.HashBytes([]byte("x")),
		Size:       10,
	})
	require.NoError(t, err)

	// Delete
	err = idx.DeleteArtifact(ctx, coord)
	require.NoError(t, err)

	// Get should fail
	_, err = idx.GetCachedArtifact(ctx, coord)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestIndexIsMetadataExpired(t *testing.T) {
	idx, _ := newTestIndex(t)

	// Override now function for testing
	fixedTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	idx.now = func() time.Time { return fixedTime }

	meta := &CachedMetadata{
		GroupID:    "org.example",
		ArtifactID: "test",
		UpdatedAt:  fixedTime.Add(-10 * time.Minute),
	}

	// TTL of 5 minutes - should be expired
	require.True(t, idx.IsMetadataExpired(meta, 5*time.Minute))

	// TTL of 15 minutes - should not be expired
	require.False(t, idx.IsMetadataExpired(meta, 15*time.Minute))

	// TTL of 0 (disabled) - should never expire
	require.False(t, idx.IsMetadataExpired(meta, 0))
}

func TestIndexKeyGeneration(t *testing.T) {
	// Test metadata key
	key := metadataKey("org.apache.commons", "commons-lang3")
	require.Equal(t, "org/apache/commons/commons-lang3", key)

	// Test artifact key without classifier
	coord1 := ArtifactCoordinate{
		GroupID:    "org.apache.commons",
		ArtifactID: "commons-lang3",
		Version:    "3.12.0",
		Extension:  "jar",
	}
	artifactKey1 := artifactKey(coord1)
	require.Equal(t, "org/apache/commons/commons-lang3/3.12.0/_default/jar", artifactKey1)

	// Test artifact key with classifier
	coord2 := ArtifactCoordinate{
		GroupID:    "org.apache.commons",
		ArtifactID: "commons-lang3",
		Version:    "3.12.0",
		Classifier: "sources",
		Extension:  "jar",
	}
	artifactKey2 := artifactKey(coord2)
	require.Equal(t, "org/apache/commons/commons-lang3/3.12.0/sources/jar", artifactKey2)
}

func TestCollectBlobRefs(t *testing.T) {
	// Test nil artifact
	require.Nil(t, collectBlobRefs(nil))

	// Test artifact with zero hash
	artifact := &CachedArtifact{}
	require.Nil(t, collectBlobRefs(artifact))

	// Test artifact with valid hash
	hash := contentcache.HashBytes([]byte("test content"))
	artifact.Hash = hash
	refs := collectBlobRefs(artifact)
	require.Len(t, refs, 1)
	require.Equal(t, contentcache.NewBlobRef(hash).String(), refs[0])
}
