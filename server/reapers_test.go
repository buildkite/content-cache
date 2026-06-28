package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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

func TestServerStartStopsMetadataReapersWhenListenFails(t *testing.T) {
	db := metadb.NewBoltDB()
	require.NoError(t, db.Open(t.TempDir()+"/metadata.db"))
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	reapers := newMetadataReapers(
		db,
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	s := &Server{
		config: Config{
			Address:             "127.0.0.1:bad-port",
			ExpiryCheckInterval: time.Hour,
		},
		httpServer: &http.Server{
			Addr:              "127.0.0.1:bad-port",
			ReadHeaderTimeout: time.Second,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),

		metadataReapers: reapers,
	}

	require.Error(t, s.Start())

	done := make(chan struct{})
	go func() {
		reapers.done.Wait()
		close(done)
	}()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}
