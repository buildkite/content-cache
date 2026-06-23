package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
	"github.com/stretchr/testify/require"
)

func TestMetadataReapersReleaseExpiredEnvelopeBlobRefs(t *testing.T) {
	ctx := context.Background()
	db := metadb.NewBoltDB()
	require.NoError(t, db.Open(t.TempDir()+"/metadata.db"))
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	const hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, db.PutBlob(ctx, &metadb.BlobEntry{
		Hash:       hash,
		Size:       100,
		CachedAt:   time.Now(),
		LastAccess: time.Now(),
	}))
	require.NoError(t, db.PutEnvelope(ctx, "test", "artifact", "key", &metadb.MetadataEnvelope{
		EnvelopeVersion: metadb.CurrentEnvelopeVersion,
		ExpiresAtUnixMs: time.Now().Add(-time.Minute).UnixMilli(),
		BlobRefs:        []string{hash},
	}))

	blob, err := db.GetBlob(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, 1, blob.RefCount)

	reapers := newMetadataReapers(
		db,
		10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reapers.Start(ctx)
	t.Cleanup(reapers.Stop)

	require.Eventually(t, func() bool {
		_, envelopeErr := db.GetEnvelope(ctx, "test", "artifact", "key")
		blob, blobErr := db.GetBlob(ctx, hash)
		return errors.Is(envelopeErr, metadb.ErrNotFound) &&
			blobErr == nil && blob.RefCount == 0
	}, time.Second, 10*time.Millisecond)
}
