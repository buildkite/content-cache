// Package s3fifo implements the S3-FIFO cache eviction algorithm.
// See: https://www.pdl.cmu.edu/ftp/Storage/CMU-CS-24-149-juncheny.pdf
package s3fifo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/buildkite/content-cache/telemetry"
	"go.etcd.io/bbolt"
)

// ErrQueueEmpty is returned when popping from an empty queue.
var ErrQueueEmpty = errors.New("s3fifo: queue is empty")

// Queue name constants.
const (
	QueueSmall = "small"
	QueueMain  = "main"
)

// bbolt bucket names for S3-FIFO state. All are prefixed with "s3fifo_" to
// avoid collisions with metadb buckets in the same database file.
var (
	bucketSmall       = []byte("s3fifo_small")         // seq(uint64BE) → hash
	bucketSmallByHash = []byte("s3fifo_small_by_hash") // hash → seq(uint64BE)
	bucketMain        = []byte("s3fifo_main")          // seq(uint64BE) → hash
	bucketMainByHash  = []byte("s3fifo_main_by_hash")  // hash → seq(uint64BE)
	bucketGhost       = []byte("s3fifo_ghost")         // hash → seq(uint64BE)
	bucketGhostBySeq  = []byte("s3fifo_ghost_by_seq")  // seq(uint64BE) → hash
)

// Queues is the interface for S3-FIFO queue and ghost set operations.
// Implementations must be safe to call concurrently.
type Queues interface {
	PushHead(queue, hash string) (bool, error)
	PopTail(queue string) (string, error)
	Remove(queue, hash string) (bool, error)
	Len(queue string) (int, error)
	ForEach(queue string, fn func(hash string) error) error
	AdmitGhostHit(hash string) (bool, error)
	GhostContains(hash string) (bool, error)
	GhostAdd(hash string) error
	GhostRemove(hash string) error
	GhostTrimToMaxSize(maxEntries int) error
	GhostLen() (int, error)
}

// BoltQueues provides bbolt-backed FIFO queue and ghost set operations for S3-FIFO.
// All methods are safe to call concurrently (bbolt handles its own locking),
// and the S3-FIFO Manager coordinates membership changes with queueMu.
type BoltQueues struct {
	db            *bbolt.DB
	activeWriters atomic.Int64
}

// NewBoltQueues creates a BoltQueues instance backed by the given bbolt database and
// ensures all required buckets exist.
func NewBoltQueues(db *bbolt.DB) (*BoltQueues, error) {
	q := &BoltQueues{db: db}
	return q, q.createBuckets()
}

func (q *BoltQueues) batch(operation string, fn func(*bbolt.Tx) error) error {
	ctx := context.Background()
	start := time.Now()
	var callbackDuration time.Duration
	var txID int
	// A lone caller commits immediately. Concurrent callers can coalesce;
	// callbacks must be replay-safe because bbolt retries failed batches.
	active := q.activeWriters.Add(1)
	defer q.activeWriters.Add(-1)
	write := q.db.Batch
	if active == 1 || q.db.MaxBatchSize == 1 {
		write = q.db.Update
	}
	err := write(func(tx *bbolt.Tx) error {
		callbackStart := time.Now()
		txID = tx.ID()
		err := fn(tx)
		callbackDuration += time.Since(callbackStart)
		return err
	})
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	telemetry.RecordMetaDBBatch(ctx, operation, outcome, time.Since(start), callbackDuration)
	if err == nil {
		stats := q.db.Stats()
		telemetry.UpdateMetaDBStats(ctx, int64(txID), stats.TxStats.GetWrite(), stats.TxStats.GetWriteTime())
	}
	return err
}

func (q *BoltQueues) createBuckets() error {
	return q.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{
			bucketSmall, bucketSmallByHash,
			bucketMain, bucketMainByHash,
			bucketGhost, bucketGhostBySeq,
		} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

// PushHead inserts hash at the head (newest position) of the named queue.
// Subsequent PopTail calls return older entries first.
func (q *BoltQueues) PushHead(queue, hash string) (bool, error) {
	var replaced bool
	err := q.batch("s3fifo_push_head", func(tx *bbolt.Tx) error {
		var err error
		replaced, err = txPushHead(tx, queueFwdName(queue), queueRevName(queue), hash)
		return err
	})
	return replaced, err
}

// PopTail removes and returns the oldest hash from the named queue.
// Returns ErrQueueEmpty when the queue has no entries.
func (q *BoltQueues) PopTail(queue string) (string, error) {
	var hash string
	err := q.batch("s3fifo_pop_tail", func(tx *bbolt.Tx) error {
		var err error
		hash, err = txPopTail(tx, queueFwdName(queue), queueRevName(queue))
		return err
	})
	return hash, err
}

// Remove removes a specific hash from the named queue.
// Returns (true, nil) if the hash was present and removed, (false, nil) if absent.
func (q *BoltQueues) Remove(queue, hash string) (bool, error) {
	var removed bool
	err := q.batch("s3fifo_remove", func(tx *bbolt.Tx) error {
		removed = false
		fwd := tx.Bucket(queueFwdName(queue))
		rev := tx.Bucket(queueRevName(queue))

		seqVal := rev.Get([]byte(hash))
		if seqVal == nil {
			return nil
		}
		removed = true

		seqKey := make([]byte, len(seqVal))
		copy(seqKey, seqVal)

		if err := fwd.Delete(seqKey); err != nil {
			return err
		}
		return rev.Delete([]byte(hash))
	})
	return removed, err
}

// Len returns the number of entries in the named queue.
func (q *BoltQueues) Len(queue string) (int, error) {
	var count int
	err := q.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(queueFwdName(queue))
		if b != nil {
			count = b.Stats().KeyN
		}
		return nil
	})
	return count, err
}

// ForEach iterates all entries in a queue in FIFO order (oldest first) within a
// read-only transaction. fn must not perform any bbolt writes.
func (q *BoltQueues) ForEach(queue string, fn func(hash string) error) error {
	return q.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(queueFwdName(queue))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			return fn(string(v))
		})
	})
}

// AdmitGhostHit atomically removes hash from the ghost set and inserts it at
// the head of the main queue. It reports whether the hash was still present;
// concurrent admissions can observe the same ghost entry before the batch runs.
func (q *BoltQueues) AdmitGhostHit(hash string) (bool, error) {
	var admitted bool
	err := q.batch("s3fifo_ghost_hit", func(tx *bbolt.Tx) error {
		admitted = false
		ghostBucket := tx.Bucket(bucketGhost)
		seqBucket := tx.Bucket(bucketGhostBySeq)

		seqVal := ghostBucket.Get([]byte(hash))
		if seqVal == nil {
			// Another concurrent admission already consumed the ghost entry.
			return nil
		}
		admitted = true
		seqKey := make([]byte, len(seqVal))
		copy(seqKey, seqVal)

		if err := ghostBucket.Delete([]byte(hash)); err != nil {
			return err
		}
		if err := seqBucket.Delete(seqKey); err != nil {
			return err
		}

		_, err := txPushHead(tx, bucketMain, bucketMainByHash, hash)
		return err
	})
	return admitted, err
}

// GhostContains reports whether hash is currently in the ghost set.
func (q *BoltQueues) GhostContains(hash string) (bool, error) {
	var found bool
	err := q.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGhost)
		if b != nil {
			found = b.Get([]byte(hash)) != nil
		}
		return nil
	})
	return found, err
}

// GhostAdd inserts hash into the ghost set with the next sequence number.
func (q *BoltQueues) GhostAdd(hash string) error {
	return q.batch("s3fifo_ghost_add", func(tx *bbolt.Tx) error {
		ghostBucket := tx.Bucket(bucketGhost)
		seqBucket := tx.Bucket(bucketGhostBySeq)

		if oldSeq := ghostBucket.Get([]byte(hash)); oldSeq != nil {
			oldSeqKey := make([]byte, len(oldSeq))
			copy(oldSeqKey, oldSeq)
			if err := seqBucket.Delete(oldSeqKey); err != nil {
				return err
			}
		}

		seq, err := seqBucket.NextSequence()
		if err != nil {
			return err
		}
		seqKey := encodeSeq(seq)

		if err := ghostBucket.Put([]byte(hash), seqKey); err != nil {
			return err
		}
		return seqBucket.Put(seqKey, []byte(hash))
	})
}

// GhostRemove removes hash from the ghost set. No-op if not present.
func (q *BoltQueues) GhostRemove(hash string) error {
	return q.batch("s3fifo_ghost_remove", func(tx *bbolt.Tx) error {
		ghostBucket := tx.Bucket(bucketGhost)
		seqBucket := tx.Bucket(bucketGhostBySeq)

		seqVal := ghostBucket.Get([]byte(hash))
		if seqVal == nil {
			return nil
		}
		seqKey := make([]byte, len(seqVal))
		copy(seqKey, seqVal)

		if err := ghostBucket.Delete([]byte(hash)); err != nil {
			return err
		}
		return seqBucket.Delete(seqKey)
	})
}

// GhostTrimToMaxSize evicts the oldest ghost entries until the count is at or
// below maxEntries. Each trim step is O(1) using the ordered seq index.
func (q *BoltQueues) GhostTrimToMaxSize(maxEntries int) error {
	return q.db.Update(func(tx *bbolt.Tx) error {
		ghostBucket := tx.Bucket(bucketGhost)
		seqBucket := tx.Bucket(bucketGhostBySeq)

		// Stats().KeyN reflects committed state and does not update within the
		// current transaction, so we track the count with a local variable.
		count := seqBucket.Stats().KeyN
		for count > maxEntries {
			c := seqBucket.Cursor()
			seqKey, hashVal := c.First()
			if seqKey == nil {
				break
			}
			seqKeyCopy := make([]byte, len(seqKey))
			copy(seqKeyCopy, seqKey)
			hashCopy := make([]byte, len(hashVal))
			copy(hashCopy, hashVal)

			if err := seqBucket.Delete(seqKeyCopy); err != nil {
				return err
			}
			if err := ghostBucket.Delete(hashCopy); err != nil {
				return err
			}
			count--
		}
		return nil
	})
}

// GhostLen returns the current number of entries in the ghost set.
func (q *BoltQueues) GhostLen() (int, error) {
	var count int
	err := q.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGhost)
		if b != nil {
			count = b.Stats().KeyN
		}
		return nil
	})
	return count, err
}

// --- internal helpers ---

// txPushHead inserts hash into the queue using the next monotonic sequence number.
// Must be called inside a bbolt Update transaction.
func txPushHead(tx *bbolt.Tx, fwdName, revName []byte, hash string) (bool, error) {
	fwd := tx.Bucket(fwdName)
	rev := tx.Bucket(revName)

	replaced := false
	if oldSeq := rev.Get([]byte(hash)); oldSeq != nil {
		replaced = true
		oldSeqKey := make([]byte, len(oldSeq))
		copy(oldSeqKey, oldSeq)
		if err := fwd.Delete(oldSeqKey); err != nil {
			return false, err
		}
	}

	seq, err := fwd.NextSequence()
	if err != nil {
		return false, err
	}
	seqKey := encodeSeq(seq)

	if err := fwd.Put(seqKey, []byte(hash)); err != nil {
		return false, err
	}
	return replaced, rev.Put([]byte(hash), seqKey)
}

// txPopTail removes and returns the entry with the lowest sequence number
// (oldest / FIFO tail). Must be called inside a bbolt Update transaction.
func txPopTail(tx *bbolt.Tx, fwdName, revName []byte) (string, error) {
	fwd := tx.Bucket(fwdName)
	rev := tx.Bucket(revName)

	c := fwd.Cursor()
	k, v := c.First()
	if k == nil {
		return "", ErrQueueEmpty
	}

	// Copy before any mutation invalidates the cursor memory.
	seqKey := make([]byte, len(k))
	copy(seqKey, k)
	hashVal := make([]byte, len(v))
	copy(hashVal, v)

	if err := fwd.Delete(seqKey); err != nil {
		return "", err
	}
	if err := rev.Delete(hashVal); err != nil {
		return "", err
	}
	return string(hashVal), nil
}

// queueFwdName returns the forward bucket name (seq→hash) for a named queue.
func queueFwdName(queue string) []byte {
	switch queue {
	case QueueSmall:
		return bucketSmall
	case QueueMain:
		return bucketMain
	default:
		panic("s3fifo: unknown queue name: " + queue)
	}
}

// queueRevName returns the reverse bucket name (hash→seq) for a named queue.
func queueRevName(queue string) []byte {
	switch queue {
	case QueueSmall:
		return bucketSmallByHash
	case QueueMain:
		return bucketMainByHash
	default:
		panic("s3fifo: unknown queue name: " + queue)
	}
}

// encodeSeq encodes a uint64 sequence number as a big-endian 8-byte slice
// for lexicographic ordering in bbolt.
func encodeSeq(seq uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, seq)
	return buf
}
