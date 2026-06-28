package metadb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setupEnvelopeTestDB(t *testing.T) *BoltDB {
	t.Helper()
	db := NewBoltDB(WithNoSync(true))
	path := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, db.Open(path))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnvelope_PutGetRoundTrip(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		ContentEncoding: ContentEncoding_CONTENT_ENCODING_IDENTITY,
		Payload:         []byte(`{"name":"test"}`),
		PayloadDigest:   "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		PayloadSize:     15,
		FetchedAtUnixMs: time.Now().UnixMilli(),
		ExpiresAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		TtlSeconds:      3600,
		Upstream:        "registry.npmjs.org",
		UpstreamStatus:  200,
	}

	err := db.PutEnvelope(ctx, "npm", "metadata", "lodash", env)
	require.NoError(t, err)

	got, err := db.GetEnvelope(ctx, "npm", "metadata", "lodash")
	require.NoError(t, err)
	require.Equal(t, env.EnvelopeVersion, got.EnvelopeVersion)
	require.Equal(t, env.ContentType, got.ContentType)
	require.Equal(t, env.Payload, got.Payload)
	require.Equal(t, env.Upstream, got.Upstream)
	require.Equal(t, env.UpstreamStatus, got.UpstreamStatus)
}

func TestEnvelope_NotFound(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	_, err := db.GetEnvelope(ctx, "npm", "metadata", "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEnvelope_Delete(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "to-delete", env))

	_, err := db.GetEnvelope(ctx, "npm", "metadata", "to-delete")
	require.NoError(t, err)

	require.NoError(t, db.DeleteEnvelope(ctx, "npm", "metadata", "to-delete"))

	_, err = db.GetEnvelope(ctx, "npm", "metadata", "to-delete")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEnvelope_ListKeys(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg-a", env))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg-b", env))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg-c", env))
	require.NoError(t, db.PutEnvelope(ctx, "pypi", "project", "requests", env))

	keys, err := db.ListEnvelopeKeys(ctx, "npm", "metadata")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"pkg-a", "pkg-b", "pkg-c"}, keys)

	keys, err = db.ListEnvelopeKeys(ctx, "pypi", "project")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"requests"}, keys)

	keys, err = db.ListEnvelopeKeys(ctx, "oci", "manifest")
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestEnvelope_BlobRefTracking(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	hash1 := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	hash2 := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash1, Size: 100}))
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash2, Size: 200}))

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash1},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	blob1, err := db.GetBlob(ctx, hash1)
	require.NoError(t, err)
	require.Equal(t, 1, blob1.RefCount)

	env.BlobRefs = []string{hash1, hash2}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	blob1, _ = db.GetBlob(ctx, hash1)
	require.Equal(t, 1, blob1.RefCount)
	blob2, _ := db.GetBlob(ctx, hash2)
	require.Equal(t, 1, blob2.RefCount)

	env.BlobRefs = []string{hash2}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	blob1, _ = db.GetBlob(ctx, hash1)
	require.Equal(t, 0, blob1.RefCount)
	blob2, _ = db.GetBlob(ctx, hash2)
	require.Equal(t, 1, blob2.RefCount)

	require.NoError(t, db.DeleteEnvelope(ctx, "npm", "metadata", "pkg"))

	blob2, _ = db.GetBlob(ctx, hash2)
	require.Equal(t, 0, blob2.RefCount)
}

func TestEnvelope_ExpiryIndex(t *testing.T) {
	now := time.Now()
	db := NewBoltDB(WithNoSync(true), WithNow(func() time.Time { return now }))
	path := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, db.Open(path))
	defer db.Close()
	ctx := context.Background()

	env1 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		ExpiresAtUnixMs: now.Add(-time.Hour).UnixMilli(),
	}
	env2 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		ExpiresAtUnixMs: now.Add(-30 * time.Minute).UnixMilli(),
	}
	env3 := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		ExpiresAtUnixMs: now.Add(time.Hour).UnixMilli(),
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "expired-1", env1))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "expired-2", env2))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "not-expired", env3))

	expired, err := db.GetExpiredEnvelopes(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, expired, 2)
	require.Equal(t, "expired-1", expired[0].Key)
	require.Equal(t, "expired-2", expired[1].Key)
}

func TestEnvelope_BatchDeleteExpired(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	now := time.Now()
	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		ExpiresAtUnixMs: now.Add(-time.Hour).UnixMilli(),
	}

	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg1", env))
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg2", env))

	entries, err := db.GetExpiredEnvelopes(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	require.NoError(t, db.DeleteExpiredEnvelopes(ctx, entries))

	keys, err := db.ListEnvelopeKeys(ctx, "npm", "metadata")
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestEnvelope_BatchDeleteExpiredSkipsRefreshedEnvelope(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	oldHash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newHash := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: oldHash, Size: 100}))
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: newHash, Size: 200}))

	now := time.Now()
	expired := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{"version":1}`),
		ExpiresAtUnixMs: now.Add(-time.Hour).UnixMilli(),
		BlobRefs:        []string{oldHash},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", expired))

	entries, err := db.GetExpiredEnvelopes(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	refreshed := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{"version":2}`),
		ExpiresAtUnixMs: now.Add(time.Hour).UnixMilli(),
		BlobRefs:        []string{newHash},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", refreshed))

	require.NoError(t, db.DeleteExpiredEnvelopes(ctx, entries))

	env, err := db.GetEnvelope(ctx, "npm", "metadata", "pkg")
	require.NoError(t, err)
	require.Equal(t, []byte(`{"version":2}`), env.Payload)

	oldBlob, err := db.GetBlob(ctx, oldHash)
	require.NoError(t, err)
	require.Equal(t, 0, oldBlob.RefCount)

	newBlob, err := db.GetBlob(ctx, newHash)
	require.NoError(t, err)
	require.Equal(t, 1, newBlob.RefCount)
}

func TestEnvelope_UpdateModify(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		ContentEncoding: ContentEncoding_CONTENT_ENCODING_IDENTITY,
		Payload:         []byte(`{"count":1}`),
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "counter", env))

	err := db.UpdateEnvelope(ctx, "npm", "metadata", "counter", func(existing *MetadataEnvelope) (*MetadataEnvelope, error) {
		require.NotNil(t, existing)
		existing.Payload = []byte(`{"count":2}`)
		return existing, nil
	})
	require.NoError(t, err)

	got, err := db.GetEnvelope(ctx, "npm", "metadata", "counter")
	require.NoError(t, err)
	require.Equal(t, []byte(`{"count":2}`), got.Payload)
}

func TestEnvelope_UpdateCreate(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	err := db.UpdateEnvelope(ctx, "npm", "metadata", "new-key", func(existing *MetadataEnvelope) (*MetadataEnvelope, error) {
		require.Nil(t, existing)
		return &MetadataEnvelope{
			EnvelopeVersion: 1,
			ContentType:     ContentType_CONTENT_TYPE_JSON,
			Payload:         []byte(`{"created":true}`),
		}, nil
	})
	require.NoError(t, err)

	got, err := db.GetEnvelope(ctx, "npm", "metadata", "new-key")
	require.NoError(t, err)
	require.Equal(t, []byte(`{"created":true}`), got.Payload)
}

func TestEnvelope_UpdateDelete(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "to-delete", env))

	err := db.UpdateEnvelope(ctx, "npm", "metadata", "to-delete", func(_ *MetadataEnvelope) (*MetadataEnvelope, error) {
		return nil, nil
	})
	require.NoError(t, err)

	_, err = db.GetEnvelope(ctx, "npm", "metadata", "to-delete")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEnvelope_ValidationRejectsInvalidRefs(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{"invalid-ref"},
	}

	err := db.PutEnvelope(ctx, "npm", "metadata", "pkg", env)
	require.ErrorIs(t, err, ErrInvalidDigestFormat)
}

func TestEnvelope_RefsCanonicalized(t *testing.T) {
	db := setupEnvelopeTestDB(t)
	ctx := context.Background()

	hash := "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	hashLower := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hashLower, Size: 100}))

	env := &MetadataEnvelope{
		EnvelopeVersion: 1,
		ContentType:     ContentType_CONTENT_TYPE_JSON,
		Payload:         []byte(`{}`),
		BlobRefs:        []string{hash, hash},
	}
	require.NoError(t, db.PutEnvelope(ctx, "npm", "metadata", "pkg", env))

	refs, err := db.GetEnvelopeBlobRefs(ctx, "npm", "metadata", "pkg")
	require.NoError(t, err)
	require.Equal(t, []string{hashLower}, refs)
}

func TestEnvelopeKey_RoundTrip(t *testing.T) {
	tests := []struct {
		protocol string
		kind     string
		key      string
	}{
		{"npm", "metadata", "lodash"},
		{"goproxy", "mod", "github.com/foo/bar@v1.0.0"},
		{"oci", "manifest", "library/nginx@sha256:abc123"},
		{"pypi", "project", "requests"},
	}

	for _, tt := range tests {
		t.Run(tt.protocol+"/"+tt.kind+"/"+tt.key, func(t *testing.T) {
			encoded := makeEnvelopeKey(tt.protocol, tt.kind, tt.key)
			protocol, kind, key := parseEnvelopeKey(encoded)
			require.Equal(t, tt.protocol, protocol)
			require.Equal(t, tt.kind, kind)
			require.Equal(t, tt.key, key)
		})
	}
}
