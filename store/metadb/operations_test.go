package metadb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setupOperationsTestDB(t *testing.T) *BoltDB {
	t.Helper()
	db := NewBoltDB(WithNoSync(true))
	path := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, db.Open(path))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestVerifyEnvelopeRefcounts_NoDiscrepancies(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	hash1 := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	hash2 := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash1, Size: 100}))
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash2, Size: 200}))

	env1 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash1},
	}
	env2 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash1, hash2},
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg1", env1))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg2", env2))

	discrepancies, err := db.VerifyEnvelopeRefcounts(ctx)
	require.NoError(t, err)
	require.Empty(t, discrepancies)
}

func TestVerifyEnvelopeRefcounts_DetectsDiscrepancy(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	hash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash, Size: 100, RefCount: 5}))

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	discrepancies, err := db.VerifyEnvelopeRefcounts(ctx)
	require.NoError(t, err)
	require.Len(t, discrepancies, 1)
	require.Equal(t, hash, discrepancies[0].Hash)
	require.Equal(t, 6, discrepancies[0].Stored)
	require.Equal(t, 1, discrepancies[0].Computed)
}

func TestVerifyEnvelopeRefcounts_DetectsMissingBlob(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	hash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	discrepancies, err := db.VerifyEnvelopeRefcounts(ctx)
	require.NoError(t, err)
	require.Len(t, discrepancies, 1)
	require.Equal(t, hash, discrepancies[0].Hash)
	require.Equal(t, 0, discrepancies[0].Stored)
	require.Equal(t, 1, discrepancies[0].Computed)
}

func TestRebuildEnvelopeRefcounts(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	hash1 := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	hash2 := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash1, Size: 100}))
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash2, Size: 200}))

	env1 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash1},
	}
	env2 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash1, hash2},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg1", env1))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg2", env2))

	blob1, _ := db.GetBlob(ctx, hash1)
	blob1.RefCount = 99
	require.NoError(t, db.PutBlob(ctx, blob1))

	blob2, _ := db.GetBlob(ctx, hash2)
	blob2.RefCount = 50
	require.NoError(t, db.PutBlob(ctx, blob2))

	discrepancies, _ := db.VerifyEnvelopeRefcounts(ctx)
	require.NotEmpty(t, discrepancies, "should have discrepancies after manual refcount corruption")

	updated, err := db.RebuildEnvelopeRefcounts(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, updated)

	discrepancies, err = db.VerifyEnvelopeRefcounts(ctx)
	require.NoError(t, err)
	require.Empty(t, discrepancies)

	blob1, _ = db.GetBlob(ctx, hash1)
	require.Equal(t, 2, blob1.RefCount)

	blob2, _ = db.GetBlob(ctx, hash2)
	require.Equal(t, 1, blob2.RefCount)
}

func TestEnvelopeStats(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	now := time.Now()

	env1 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		ContentEncoding: ContentEncoding_CONTENT_ENCODING_IDENTITY,
		Payload:         []byte(`{"name":"test"}`),
		PayloadSize:     15,
		FetchedAtUnixMs: now.Add(-time.Hour).UnixMilli(),
		ExpiresAtUnixMs: now.Add(time.Hour).UnixMilli(),
	}
	env2 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		ContentEncoding: ContentEncoding_CONTENT_ENCODING_ZSTD,
		Payload:         []byte(`compressed`),
		PayloadSize:     96 * 1024 * 1024,
		FetchedAtUnixMs: now.UnixMilli(),
		ExpiresAtUnixMs: now.Add(-time.Minute).UnixMilli(),
	}
	env3 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		UpstreamStatus:  404,
		FetchedAtUnixMs: now.Add(-30 * time.Minute).UnixMilli(),
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg1", env1))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg2", env2))
	require.NoError(t, db.PutEnvelope(ctx, "pypi", "project", "requests", env3))

	hash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash, Size: 100}))

	stats, err := db.EnvelopeStats(ctx)
	require.NoError(t, err)

	require.Equal(t, int64(3), stats.EnvelopeCount)
	require.Equal(t, int64(1), stats.BlobCount)
	require.Equal(t, int64(1), stats.ExpiredCount)
	require.Equal(t, int64(96*1024*1024+15), stats.TotalPayloadSize)
	require.Equal(t, int64(1), stats.CompressedCount)
	require.Equal(t, int64(1), stats.NegativeCacheCount)

	require.Equal(t, int64(2), stats.ByProtocol["npm"])
	require.Equal(t, int64(1), stats.ByProtocol["pypi"])
	require.Equal(t, int64(2), stats.ByKind["metadata"])
	require.Equal(t, int64(1), stats.ByKind["project"])
	require.Equal(t, int64(2), stats.ByProtocolKind["npm/metadata"])
	require.Equal(t, int64(1), stats.ByProtocolKind["pypi/project"])

	require.False(t, stats.OldestFetchedAt.IsZero())
	require.False(t, stats.NewestFetchedAt.IsZero())
	require.True(t, stats.OldestFetchedAt.Before(stats.NewestFetchedAt))
}

func TestInspectEnvelope(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	t.Run("existing envelope", func(t *testing.T) {
		now := time.Now()
		hash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

		env := &MetadataEnvelope{
			EnvelopeVersion:    1,
			ContentType:        ContentType_CONTENT_TYPE_JSON,
			ContentEncoding:    ContentEncoding_CONTENT_ENCODING_IDENTITY,
			Payload:            []byte(`{"name":"lodash"}`),
			PayloadSize:        17,
			PayloadDigest:      "sha256:abc123",
			FetchedAtUnixMs:    now.UnixMilli(),
			ExpiresAtUnixMs:    now.Add(time.Hour).UnixMilli(),
			TtlSeconds:         3600,
			Etag:               `"v4.17.21"`,
			LastModifiedUnixMs: now.Add(-24 * time.Hour).UnixMilli(),
			Upstream:           "registry.npmjs.org",
			UpstreamStatus:     200,
			BlobRefs:           []string{hash},
			Attributes:         map[string][]byte{"custom": []byte("value")},
		}
		require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "lodash", env))

		result, err := db.InspectEnvelope(ctx, "npm", "metadata", "lodash")
		require.NoError(t, err)

		require.True(t, result.Exists)
		require.Equal(t, "npm", result.Protocol)
		require.Equal(t, "metadata", result.Kind)
		require.Equal(t, "lodash", result.Key)
		require.Equal(t, uint32(1), result.EnvelopeVersion)
		require.Contains(t, result.ContentType, "JSON")
		require.Contains(t, result.ContentEncoding, "IDENTITY")
		require.Equal(t, uint64(17), result.PayloadSize)
		require.Equal(t, 17, result.CompressedSize)
		require.InDelta(t, 1.0, result.CompressionRatio, 0.001)
		require.Equal(t, "sha256:abc123", result.PayloadDigest)
		require.Equal(t, int64(3600), result.TTLSeconds)
		require.Equal(t, `"v4.17.21"`, result.Etag)
		require.Equal(t, "registry.npmjs.org", result.Upstream)
		require.Equal(t, uint32(200), result.UpstreamStatus)
		require.False(t, result.IsNegativeCache)
		require.False(t, result.IsExpired)
		require.Equal(t, []string{hash}, result.BlobRefs)
		require.Contains(t, result.AttributeKeys, "custom")
	})

	t.Run("nonexistent envelope", func(t *testing.T) {
		result, err := db.InspectEnvelope(ctx, "npm", "metadata", "nonexistent")
		require.NoError(t, err)
		require.False(t, result.Exists)
		require.Equal(t, "npm", result.Protocol)
		require.Equal(t, "metadata", result.Kind)
		require.Equal(t, "nonexistent", result.Key)
	})

	t.Run("expired envelope", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			Payload:         []byte(`{}`),
			ExpiresAtUnixMs: time.Now().Add(-time.Hour).UnixMilli(),
		}
		require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "expired", env))

		result, err := db.InspectEnvelope(ctx, "npm", "metadata", "expired")
		require.NoError(t, err)
		require.True(t, result.Exists)
		require.True(t, result.IsExpired)
	})

	t.Run("negative cache entry", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			UpstreamStatus:  404,
		}
		require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "notfound", env))

		result, err := db.InspectEnvelope(ctx, "npm", "metadata", "notfound")
		require.NoError(t, err)
		require.True(t, result.Exists)
		require.True(t, result.IsNegativeCache)
	})
}

func TestListEnvelopeProtocols(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{EnvelopeVersion: 1, Payload: []byte(`{}`)}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg1", env))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg2", env))
	require.NoError(t, db.PutEnvelope(ctx, "pypi", "project", "req", env))
	require.NoError(t, db.PutEnvelope(ctx, "goproxy", "mod", "mod1", env))
	require.NoError(t, db.PutEnvelope(ctx, "goproxy", "info", "mod1", env))

	protocols, err := db.ListEnvelopeProtocols(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"npm/metadata",
		"pypi/project",
		"goproxy/mod",
		"goproxy/info",
	}, protocols)
}

func TestCompactDB(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			Payload:         []byte(`{"data":"` + string(rune('a'+i%26)) + `"}`),
		}
		require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", string(rune('a'+i%26))+string(rune('0'+i/26)), env))
	}

	for i := 0; i < 50; i++ {
		require.NoError(t, db.DeleteEnvelope(ctx, "npm", "metadata", string(rune('a'+i%26))+string(rune('0'+i/26))))
	}

	destPath := filepath.Join(t.TempDir(), "compacted.db")
	require.NoError(t, db.CompactDB(ctx, destPath))

	compactedDB := NewBoltDB(WithNoSync(true))
	require.NoError(t, compactedDB.Open(destPath))
	defer compactedDB.Close()

	keys, err := compactedDB.ListEnvelopeKeys(ctx, "npm", "metadata")
	require.NoError(t, err)
	require.Len(t, keys, 50)
}

func TestExportEnvelopesToJSON(t *testing.T) {
	db := setupOperationsTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{"name":"test"}`),
		PayloadSize:     15,
		Upstream:        "test.registry",
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "test-pkg", env))

	exportPath := filepath.Join(t.TempDir(), "export.json")
	require.NoError(t, db.ExportEnvelopesToJSON(ctx, exportPath))

	data, err := os.ReadFile(exportPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "npm")
	require.Contains(t, string(data), "metadata")
	require.Contains(t, string(data), "test-pkg")
	require.Contains(t, string(data), "test.registry")
	require.NotContains(t, string(data), `"payload"`)
}
