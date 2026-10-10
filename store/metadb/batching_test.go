package metadb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// A failed member rolls back the entire bbolt batch, then successful members
// are retried. Reference counters and expiry indexes must commit exactly once.
func TestBatchRollbackPreservesRefsAndExpiry(t *testing.T) {
	db := newTestBoltDB(t, WithBatchSize(4), WithBatchDelay(time.Second))
	ctx := context.Background()
	hash := "blake3:" + fmt.Sprintf("%064x", 1)
	require.NoError(t, db.PutBlob(ctx, &BlobEntry{Hash: hash, Size: 10}))
	entered, release := make(chan struct{}), make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- db.batch(ctx, "leader", func(*bbolt.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	sentinel := errors.New("reject this batch member")
	var attempts atomic.Int64
	results := make(chan error, 4)
	for i := range 3 {
		go func() {
			results <- db.PutEnvelope(ctx, "npm", "metadata", fmt.Sprintf("pkg-%d", i), &MetadataEnvelope{
				EnvelopeVersion: CurrentEnvelopeVersion, Payload: []byte("data"), PayloadSize: 4,
				BlobRefs: []string{hash}, ExpiresAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
			})
		}()
	}
	go func() {
		results <- db.batch(ctx, "reject", func(tx *bbolt.Tx) error {
			attempts.Add(1)
			if err := tx.Bucket(bucketMeta).Put([]byte("uncommitted"), []byte("bad")); err != nil {
				return err
			}
			return sentinel
		})
	}()
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if db.activeWriters.Load() == 5 {
			ready = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	require.True(t, ready, "all writers must overlap the leader transaction")
	require.NoError(t, <-leaderDone)
	var failures int
	for range 4 {
		err := <-results
		if err != nil {
			require.ErrorIs(t, err, sentinel)
			failures++
		}
	}
	require.Equal(t, 1, failures)
	require.GreaterOrEqual(t, attempts.Load(), int64(2), "failed Batch callback must exercise bbolt retry")
	entry, err := db.GetBlob(ctx, hash)
	require.NoError(t, err)
	require.EqualValues(t, 3, entry.RefCount)
	require.NoError(t, db.DB().View(func(tx *bbolt.Tx) error {
		require.Nil(t, tx.Bucket(bucketMeta).Get([]byte("uncommitted")))
		require.Equal(t, 3, tx.Bucket(bucketEnvelopeByExpiry).Stats().KeyN)
		return nil
	}))
	for i := range 3 {
		require.NoError(t, db.DeleteEnvelope(ctx, "npm", "metadata", fmt.Sprintf("pkg-%d", i)))
	}
	entry, err = db.GetBlob(ctx, hash)
	require.NoError(t, err)
	require.Zero(t, entry.RefCount)
}

func TestLoneWriteDoesNotWaitForBatchDelay(t *testing.T) {
	db := newTestBoltDB(t, WithBatchDelay(10*time.Second))
	done := make(chan error, 1)
	go func() { done <- db.PutMeta(context.Background(), "npm", "pkg", []byte("data"), time.Hour) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("lone write waited for batch collection")
	}
}
