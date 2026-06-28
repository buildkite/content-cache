package metadb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// BoltDB implements MetaDB using bbolt.
type BoltDB struct {
	db     *bbolt.DB
	codec  *EnvelopeCodec // shared codec for all EnvelopeIndex instances
	logger *slog.Logger
	now    func() time.Time
	noSync bool // disables fsync per transaction (for testing only)
}

// BoltDBOption configures a BoltDB instance.
type BoltDBOption func(*BoltDB)

// WithLogger sets the logger for the database.
func WithLogger(logger *slog.Logger) BoltDBOption {
	return func(b *BoltDB) {
		b.logger = logger
	}
}

// WithNow sets the time function for testing.
func WithNow(now func() time.Time) BoltDBOption {
	return func(b *BoltDB) {
		b.now = now
	}
}

// WithNoSync disables fsync per transaction.
// WARNING: This improves write performance but risks data loss on crash.
// Use only for testing or benchmarking, never in production.
func WithNoSync(noSync bool) BoltDBOption {
	return func(b *BoltDB) {
		b.noSync = noSync
	}
}

// NewBoltDB creates a new BoltDB instance with options.
func NewBoltDB(opts ...BoltDBOption) *BoltDB {
	b := &BoltDB{
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Open opens the database at the given path.
func (b *BoltDB) Open(path string) error {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{
		Timeout: 1 * time.Second,
		NoSync:  b.noSync,
	})
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	b.db = db

	if err := b.createBuckets(); err != nil {
		_ = db.Close()
		return err
	}

	// Initialize shared codec for envelope operations
	codec, err := NewEnvelopeCodec()
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("creating envelope codec: %w", err)
	}
	b.codec = codec

	b.logger.Debug("opened metadb", "path", path, "noSync", b.noSync)
	return nil
}

func (b *BoltDB) createBuckets() error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		buckets := [][]byte{
			bucketMeta,
			bucketBlobsByHash,
			bucketMetaByExpiry,
			bucketMetaExpiryByKey,
			bucketMetaBlobRefs,
			// New envelope buckets
			bucketEnvelopes,
			bucketEnvelopeByExpiry,
			bucketEnvelopeExpiryByKey,
			bucketEnvelopeBlobRefs,
		}
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

// Close closes the database and releases resources.
func (b *BoltDB) Close() error {
	if b.codec != nil {
		b.codec.Close()
		b.codec = nil
	}
	if b.db == nil {
		return nil
	}
	b.logger.Debug("closing metadb")
	return b.db.Close()
}

// Codec returns the shared envelope codec.
func (b *BoltDB) Codec() *EnvelopeCodec {
	return b.codec
}

// DB returns the underlying bbolt database.
// Used by the s3fifo package to manage its queue buckets directly.
func (b *BoltDB) DB() *bbolt.DB {
	return b.db
}

// GetMeta retrieves protocol metadata.
func (b *BoltDB) GetMeta(_ context.Context, protocol, key string) ([]byte, error) {
	var data []byte
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		if bucket == nil {
			return ErrNotFound
		}

		compoundKey := makeProtocolKey(protocol, key)
		val := bucket.Get(compoundKey)
		if val == nil {
			return ErrNotFound
		}

		data = make([]byte, len(val))
		copy(data, val)
		return nil
	})
	return data, err
}

// PutMeta stores protocol metadata with TTL.
func (b *BoltDB) PutMeta(_ context.Context, protocol, key string, data []byte, ttl time.Duration) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		metaBucket := tx.Bucket(bucketMeta)
		if metaBucket == nil {
			return fmt.Errorf("meta bucket not found")
		}

		compoundKey := makeProtocolKey(protocol, key)

		// Store the data
		if err := metaBucket.Put(compoundKey, data); err != nil {
			return fmt.Errorf("putting meta: %w", err)
		}

		// Update expiry index (removes old entry, adds new if ttl > 0)
		var expiresAt *time.Time
		if ttl > 0 {
			t := b.now().Add(ttl)
			expiresAt = &t
		}
		if err := b.updateMetaExpiryIndex(tx, protocol, key, expiresAt); err != nil {
			return err
		}

		return nil
	})
}

func (b *BoltDB) removeMetaExpiryIndex(tx *bbolt.Tx, protocol, key string) error {
	return b.updateMetaExpiryIndex(tx, protocol, key, nil)
}

// updateMetaExpiryIndex updates the meta expiry forward+reverse indexes.
// If expiresAt is nil, only deletes existing index entries.
func (b *BoltDB) updateMetaExpiryIndex(tx *bbolt.Tx, protocol, key string, expiresAt *time.Time) error {
	expiryBucket := tx.Bucket(bucketMetaByExpiry)
	if expiryBucket == nil {
		return nil
	}

	reverseIndexBucket := tx.Bucket(bucketMetaExpiryByKey)
	if reverseIndexBucket == nil {
		return nil
	}

	compoundKey := makeProtocolKey(protocol, key)

	// Step 1-2: Delete old forward index entry via reverse index lookup (O(1)), then delete reverse index
	if tsBytes := reverseIndexBucket.Get(compoundKey); tsBytes != nil {
		oldExpiresAt := decodeTimestamp(tsBytes)
		expiryKey := makeMetaExpiryKey(oldExpiresAt, protocol, key)
		if err := expiryBucket.Delete(expiryKey); err != nil {
			return fmt.Errorf("deleting old expiry index: %w", err)
		}
		if err := reverseIndexBucket.Delete(compoundKey); err != nil {
			return fmt.Errorf("deleting reverse index: %w", err)
		}
	} else {
		// Fallback: scan for legacy entries without reverse index (self-healing migration)
		cursor := expiryBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			if bytes.Equal(v, compoundKey) {
				if err := cursor.Delete(); err != nil {
					return fmt.Errorf("deleting old expiry index: %w", err)
				}
				break
			}
		}
	}

	// Step 3-4: If new value provided, write new forward and reverse index entries
	if expiresAt != nil {
		expiryKey := makeMetaExpiryKey(*expiresAt, protocol, key)
		if err := expiryBucket.Put(expiryKey, compoundKey); err != nil {
			return fmt.Errorf("putting expiry index: %w", err)
		}
		if err := reverseIndexBucket.Put(compoundKey, encodeTimestamp(*expiresAt)); err != nil {
			return fmt.Errorf("putting expiry reverse index: %w", err)
		}
	}

	return nil
}

// DeleteMeta removes protocol metadata.
func (b *BoltDB) DeleteMeta(_ context.Context, protocol, key string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		metaBucket := tx.Bucket(bucketMeta)
		if metaBucket == nil {
			return nil
		}

		compoundKey := makeProtocolKey(protocol, key)

		// Remove expiry index
		if err := b.removeMetaExpiryIndex(tx, protocol, key); err != nil {
			return err
		}

		return metaBucket.Delete(compoundKey)
	})
}

// ListMeta returns all keys for a protocol.
func (b *BoltDB) ListMeta(_ context.Context, protocol string) ([]string, error) {
	var keys []string
	prefix := []byte(protocol + "\x00")

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
			_, key := parseProtocolKey(k)
			keys = append(keys, key)
		}
		return nil
	})
	return keys, err
}

// GetBlob retrieves blob metadata by hash.
func (b *BoltDB) GetBlob(_ context.Context, hash string) (*BlobEntry, error) {
	var entry BlobEntry
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketBlobsByHash)
		if bucket == nil {
			return ErrNotFound
		}

		val := bucket.Get([]byte(hash))
		if val == nil {
			return ErrNotFound
		}

		return json.Unmarshal(val, &entry)
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// PutBlob stores blob metadata.
func (b *BoltDB) PutBlob(_ context.Context, entry *BlobEntry) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		hashBucket := tx.Bucket(bucketBlobsByHash)
		if hashBucket == nil {
			return fmt.Errorf("blobs_by_hash bucket not found")
		}

		// Store blob entry
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshaling blob entry: %w", err)
		}

		if err := hashBucket.Put([]byte(entry.Hash), data); err != nil {
			return fmt.Errorf("putting blob: %w", err)
		}

		return nil
	})
}

// DeleteBlob removes blob metadata.
func (b *BoltDB) DeleteBlob(_ context.Context, hash string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		hashBucket := tx.Bucket(bucketBlobsByHash)
		if hashBucket == nil {
			return nil
		}

		return hashBucket.Delete([]byte(hash))
	})
}

// IncrementBlobRef increments the reference count for a blob.
func (b *BoltDB) IncrementBlobRef(ctx context.Context, hash string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketBlobsByHash)
		if bucket == nil {
			return ErrNotFound
		}

		val := bucket.Get([]byte(hash))
		if val == nil {
			return ErrNotFound
		}

		var entry BlobEntry
		if err := json.Unmarshal(val, &entry); err != nil {
			return fmt.Errorf("unmarshaling blob entry: %w", err)
		}

		entry.RefCount++

		data, err := json.Marshal(&entry)
		if err != nil {
			return fmt.Errorf("marshaling blob entry: %w", err)
		}

		return bucket.Put([]byte(hash), data)
	})
}

// DecrementBlobRef decrements the reference count for a blob.
func (b *BoltDB) DecrementBlobRef(ctx context.Context, hash string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketBlobsByHash)
		if bucket == nil {
			return ErrNotFound
		}

		val := bucket.Get([]byte(hash))
		if val == nil {
			return ErrNotFound
		}

		var entry BlobEntry
		if err := json.Unmarshal(val, &entry); err != nil {
			return fmt.Errorf("unmarshaling blob entry: %w", err)
		}

		if entry.RefCount > 0 {
			entry.RefCount--
		}

		data, err := json.Marshal(&entry)
		if err != nil {
			return fmt.Errorf("marshaling blob entry: %w", err)
		}

		return bucket.Put([]byte(hash), data)
	})
}

// TouchBlob updates the last access time for a blob and increments the access counter.
// Returns the new access count (capped at 3 for S3-FIFO).
// Returns (0, ErrNotFound) if the blob hash does not exist in the database.
func (b *BoltDB) TouchBlob(_ context.Context, hash string) (int, error) {
	var newCount int
	err := b.db.Update(func(tx *bbolt.Tx) error {
		hashBucket := tx.Bucket(bucketBlobsByHash)
		if hashBucket == nil {
			return ErrNotFound
		}

		val := hashBucket.Get([]byte(hash))
		if val == nil {
			return ErrNotFound
		}

		var entry BlobEntry
		if err := json.Unmarshal(val, &entry); err != nil {
			return fmt.Errorf("unmarshaling blob entry: %w", err)
		}

		// Update access time
		entry.LastAccess = b.now()

		// Increment access count, capped at 3 (S3-FIFO 2-bit counter)
		if entry.AccessCount < 3 {
			entry.AccessCount++
		}
		newCount = entry.AccessCount

		data, err := json.Marshal(&entry)
		if err != nil {
			return fmt.Errorf("marshaling blob entry: %w", err)
		}

		if err := hashBucket.Put([]byte(hash), data); err != nil {
			return fmt.Errorf("putting blob: %w", err)
		}

		return nil
	})
	return newCount, err
}

// TotalBlobSize returns the total size of all blobs.
func (b *BoltDB) TotalBlobSize(_ context.Context) (int64, error) {
	var total int64
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketBlobsByHash)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(_, v []byte) error {
			var entry BlobEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil // Skip invalid entries
			}
			total += entry.Size
			return nil
		})
	})
	return total, err
}

// GetExpiredMeta returns metadata entries that have expired before the given time.
func (b *BoltDB) GetExpiredMeta(_ context.Context, before time.Time, limit int) ([]ExpiryEntry, error) {
	var entries []ExpiryEntry
	beforeTs := encodeTimestamp(before)

	err := b.db.View(func(tx *bbolt.Tx) error {
		expiryBucket := tx.Bucket(bucketMetaByExpiry)
		if expiryBucket == nil {
			return nil
		}

		metaBucket := tx.Bucket(bucketMeta)

		cursor := expiryBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			// Keys are sorted by timestamp, so stop when we pass the cutoff
			if bytes.Compare(k[:8], beforeTs) >= 0 {
				break
			}

			if limit > 0 && len(entries) >= limit {
				break
			}

			expiresAt, protocol, key := parseMetaExpiryKey(k)

			entry := ExpiryEntry{
				Protocol:  protocol,
				Key:       key,
				ExpiresAt: expiresAt,
			}

			// Try to get additional metadata
			if metaBucket != nil {
				if data := metaBucket.Get(v); data != nil {
					entry.Size = int64(len(data))
				}
			}

			entries = append(entries, entry)
		}
		return nil
	})
	return entries, err
}

// GetUnreferencedBlobs returns blobs with RefCount == 0 whose last access time
// is before the given cutoff. When before is zero, no cutoff is applied and all
// unreferenced blobs are returned regardless of age.
func (b *BoltDB) GetUnreferencedBlobs(_ context.Context, before time.Time, limit int) ([]string, error) {
	var hashes []string

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketBlobsByHash)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			if limit > 0 && len(hashes) >= limit {
				return nil
			}

			var entry BlobEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil // Skip invalid entries
			}

			if entry.RefCount != 0 {
				return nil
			}

			// Retention floor: skip blobs still within the retention window.
			if !before.IsZero() {
				lastSeen := entry.CachedAt
				if entry.LastAccess.After(lastSeen) {
					lastSeen = entry.LastAccess
				}
				if lastSeen.After(before) {
					return nil
				}
			}

			hashes = append(hashes, string(k))
			return nil
		})
	})
	return hashes, err
}

// PutMetaWithRefs stores metadata and updates blob references atomically.
// It computes the diff between old and new refs:
// - added = newRefs - oldRefs → increment blob refcounts
// - removed = oldRefs - newRefs → decrement blob refcounts
// This is safe for overwrites and idempotent for repeated calls with the same refs.
func (b *BoltDB) PutMetaWithRefs(_ context.Context, protocol, key string, data []byte, ttl time.Duration, refs []string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		metaBucket := tx.Bucket(bucketMeta)
		if metaBucket == nil {
			return fmt.Errorf("meta bucket not found")
		}

		refsBucket := tx.Bucket(bucketMetaBlobRefs)
		if refsBucket == nil {
			return fmt.Errorf("meta_blob_refs bucket not found")
		}

		blobsBucket := tx.Bucket(bucketBlobsByHash)
		if blobsBucket == nil {
			return fmt.Errorf("blobs_by_hash bucket not found")
		}

		compoundKey := makeProtocolKey(protocol, key)

		// Get old refs for this meta key
		oldRefs := b.getMetaBlobRefs(refsBucket, compoundKey)

		// Compute diff
		added, removed := diffRefs(oldRefs, refs)

		// Update blob refcounts
		for _, hash := range added {
			if err := b.incrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("incrementing ref for %s: %w", hash, err)
			}
		}
		for _, hash := range removed {
			if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("decrementing ref for %s: %w", hash, err)
			}
		}

		// Store the new refs
		refsData, err := json.Marshal(refs)
		if err != nil {
			return fmt.Errorf("marshaling refs: %w", err)
		}
		if err := refsBucket.Put(compoundKey, refsData); err != nil {
			return fmt.Errorf("putting refs: %w", err)
		}

		// Store the metadata
		if err := metaBucket.Put(compoundKey, data); err != nil {
			return fmt.Errorf("putting meta: %w", err)
		}

		// Update expiry index
		var expiresAt *time.Time
		if ttl > 0 {
			t := b.now().Add(ttl)
			expiresAt = &t
		}
		if err := b.updateMetaExpiryIndex(tx, protocol, key, expiresAt); err != nil {
			return err
		}

		return nil
	})
}

// DeleteMetaWithRefs removes metadata and decrements all associated blob refs.
func (b *BoltDB) DeleteMetaWithRefs(_ context.Context, protocol, key string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		return b.deleteMetaWithRefsInTx(tx, protocol, key)
	})
}

// DeleteExpiredMetaWithRefs removes an expired legacy metadata entry if the
// current expiry index still matches the reaper's snapshot.
func (b *BoltDB) DeleteExpiredMetaWithRefs(_ context.Context, entry ExpiryEntry) (bool, error) {
	deleted := false
	err := b.db.Update(func(tx *bbolt.Tx) error {
		if !b.metaExpiryMatchesInTx(tx, entry) {
			return nil
		}
		if err := b.deleteMetaWithRefsInTx(tx, entry.Protocol, entry.Key); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

func (b *BoltDB) metaExpiryMatchesInTx(tx *bbolt.Tx, entry ExpiryEntry) bool {
	compoundKey := makeProtocolKey(entry.Protocol, entry.Key)
	expectedExpiry := encodeTimestamp(entry.ExpiresAt)

	reverseIndexBucket := tx.Bucket(bucketMetaExpiryByKey)
	if reverseIndexBucket != nil {
		if tsBytes := reverseIndexBucket.Get(compoundKey); tsBytes != nil {
			return bytes.Equal(tsBytes, expectedExpiry)
		}
	}

	expiryBucket := tx.Bucket(bucketMetaByExpiry)
	if expiryBucket == nil {
		return false
	}
	expiryKey := makeMetaExpiryKey(entry.ExpiresAt, entry.Protocol, entry.Key)
	return bytes.Equal(expiryBucket.Get(expiryKey), compoundKey)
}

func (b *BoltDB) deleteMetaWithRefsInTx(tx *bbolt.Tx, protocol, key string) error {
	metaBucket := tx.Bucket(bucketMeta)
	if metaBucket == nil {
		return nil
	}

	refsBucket := tx.Bucket(bucketMetaBlobRefs)
	blobsBucket := tx.Bucket(bucketBlobsByHash)

	compoundKey := makeProtocolKey(protocol, key)

	// Get refs for this meta key and decrement all
	if refsBucket != nil && blobsBucket != nil {
		refs := b.getMetaBlobRefs(refsBucket, compoundKey)
		for _, hash := range refs {
			if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
				// Log but don't fail - ref may already be gone
				b.logger.Warn("failed to decrement blob ref during delete",
					"protocol", protocol,
					"key", key,
					"hash", hash,
					"error", err)
			}
		}
		// Delete the refs entry
		if err := refsBucket.Delete(compoundKey); err != nil {
			return fmt.Errorf("deleting refs: %w", err)
		}
	}

	// Remove expiry index
	if err := b.removeMetaExpiryIndex(tx, protocol, key); err != nil {
		return err
	}

	return metaBucket.Delete(compoundKey)
}

// getMetaBlobRefs retrieves the blob refs for a meta key.
func (b *BoltDB) getMetaBlobRefs(refsBucket *bbolt.Bucket, compoundKey []byte) []string {
	val := refsBucket.Get(compoundKey)
	if val == nil {
		return nil
	}
	var refs []string
	if err := json.Unmarshal(val, &refs); err != nil {
		return nil
	}
	return refs
}

// incrementBlobRefInTx increments blob refcount within a transaction.
func (b *BoltDB) incrementBlobRefInTx(bucket *bbolt.Bucket, hash string) error {
	val := bucket.Get([]byte(hash))
	if val == nil {
		// Blob doesn't exist yet - this is OK, ref will be set when blob is stored
		return nil
	}

	var entry BlobEntry
	if err := json.Unmarshal(val, &entry); err != nil {
		return fmt.Errorf("unmarshaling blob entry: %w", err)
	}

	entry.RefCount++

	data, err := json.Marshal(&entry)
	if err != nil {
		return fmt.Errorf("marshaling blob entry: %w", err)
	}

	return bucket.Put([]byte(hash), data)
}

// decrementBlobRefInTx decrements blob refcount within a transaction.
func (b *BoltDB) decrementBlobRefInTx(bucket *bbolt.Bucket, hash string) error {
	val := bucket.Get([]byte(hash))
	if val == nil {
		return nil // Blob already deleted
	}

	var entry BlobEntry
	if err := json.Unmarshal(val, &entry); err != nil {
		return fmt.Errorf("unmarshaling blob entry: %w", err)
	}

	if entry.RefCount > 0 {
		entry.RefCount--
	}

	data, err := json.Marshal(&entry)
	if err != nil {
		return fmt.Errorf("marshaling blob entry: %w", err)
	}

	return bucket.Put([]byte(hash), data)
}

// diffRefs computes added and removed refs between old and new sets.
// added = newRefs - oldRefs (refs that were added)
// removed = oldRefs - newRefs (refs that were removed)
func diffRefs(oldRefs, newRefs []string) (added, removed []string) {
	oldSet := make(map[string]struct{}, len(oldRefs))
	newSet := make(map[string]struct{}, len(newRefs))

	for _, r := range oldRefs {
		oldSet[r] = struct{}{}
	}
	for _, r := range newRefs {
		newSet[r] = struct{}{}
	}

	for _, r := range newRefs {
		if _, ok := oldSet[r]; !ok {
			added = append(added, r)
		}
	}
	for _, r := range oldRefs {
		if _, ok := newSet[r]; !ok {
			removed = append(removed, r)
		}
	}

	return added, removed
}

// GetMetaBlobRefs returns the blob refs for a meta key.
// This is useful for testing and debugging.
func (b *BoltDB) GetMetaBlobRefs(_ context.Context, protocol, key string) ([]string, error) {
	var refs []string
	err := b.db.View(func(tx *bbolt.Tx) error {
		refsBucket := tx.Bucket(bucketMetaBlobRefs)
		if refsBucket == nil {
			return nil
		}
		compoundKey := makeProtocolKey(protocol, key)
		refs = b.getMetaBlobRefs(refsBucket, compoundKey)
		return nil
	})
	return refs, err
}

// UpdateJSON performs read-modify-write in a single Bolt transaction.
// The function fn receives the current value (or zero value if not found)
// and should modify it in place. The modified value is then stored.
// This prevents lost updates from concurrent requests.
func (b *BoltDB) UpdateJSON(_ context.Context, protocol, key string, ttl time.Duration, fn func(v any) error, v any) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		metaBucket := tx.Bucket(bucketMeta)
		if metaBucket == nil {
			return fmt.Errorf("meta bucket not found")
		}

		compoundKey := makeProtocolKey(protocol, key)

		// Read existing value
		val := metaBucket.Get(compoundKey)
		if val != nil {
			if err := json.Unmarshal(val, v); err != nil {
				return fmt.Errorf("unmarshaling existing value: %w", err)
			}
		}

		// Apply modification
		if err := fn(v); err != nil {
			return err
		}

		// Marshal and store
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshaling value: %w", err)
		}
		if err := metaBucket.Put(compoundKey, data); err != nil {
			return fmt.Errorf("putting meta: %w", err)
		}

		// Update expiry index
		var expiresAt *time.Time
		if ttl > 0 {
			t := b.now().Add(ttl)
			expiresAt = &t
		}
		return b.updateMetaExpiryIndex(tx, protocol, key, expiresAt)
	})
}

// Compile-time interface check
var _ MetaDB = (*BoltDB)(nil)

// =============================================================================
// Envelope APIs (new protocol|kind|key format with protobuf envelopes)
// =============================================================================

// PutEnvelope stores a metadata envelope with blob reference tracking.
// Handles transactional ref updates: computes diff between old and new refs,
// increments new refs, decrements removed refs.
func (b *BoltDB) PutEnvelope(_ context.Context, protocol, kind, key string, env *MetadataEnvelope) error {
	if err := ValidateEnvelope(env); err != nil {
		return fmt.Errorf("validating envelope: %w", err)
	}

	// Canonicalize refs before storage
	env.BlobRefs = CanonicalizeRefs(env.BlobRefs)

	// Marshal envelope to protobuf
	data, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling envelope: %w", err)
	}

	return b.db.Update(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket == nil {
			return fmt.Errorf("envelopes bucket not found")
		}

		refsBucket := tx.Bucket(bucketEnvelopeBlobRefs)
		if refsBucket == nil {
			return fmt.Errorf("envelope_blob_refs bucket not found")
		}

		blobsBucket := tx.Bucket(bucketBlobsByHash)
		if blobsBucket == nil {
			return fmt.Errorf("blobs_by_hash bucket not found")
		}

		compoundKey := makeEnvelopeKey(protocol, kind, key)

		// Get old refs for this key
		oldRefs := b.getEnvelopeBlobRefs(refsBucket, compoundKey)

		// Compute diff
		added, removed := DiffRefs(oldRefs, env.BlobRefs)

		// Update blob refcounts
		for _, hash := range added {
			if err := b.incrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("incrementing ref for %s: %w", hash, err)
			}
		}
		for _, hash := range removed {
			if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("decrementing ref for %s: %w", hash, err)
			}
		}

		// Store the new refs
		refsData, err := json.Marshal(env.BlobRefs)
		if err != nil {
			return fmt.Errorf("marshaling refs: %w", err)
		}
		if err := refsBucket.Put(compoundKey, refsData); err != nil {
			return fmt.Errorf("putting refs: %w", err)
		}

		// Store the envelope
		if err := envBucket.Put(compoundKey, data); err != nil {
			return fmt.Errorf("putting envelope: %w", err)
		}

		// Update expiry index
		if err := b.updateEnvelopeExpiryIndex(tx, protocol, kind, key, env.ExpiresAtUnixMs); err != nil {
			return err
		}

		return nil
	})
}

// GetEnvelope retrieves a metadata envelope.
// Returns ErrNotFound if the key doesn't exist.
func (b *BoltDB) GetEnvelope(_ context.Context, protocol, kind, key string) (*MetadataEnvelope, error) {
	var env MetadataEnvelope
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopes)
		if bucket == nil {
			return ErrNotFound
		}

		compoundKey := makeEnvelopeKey(protocol, kind, key)
		val := bucket.Get(compoundKey)
		if val == nil {
			return ErrNotFound
		}

		return proto.Unmarshal(val, &env)
	})
	if err != nil {
		return nil, err
	}
	return &env, nil
}

// DeleteEnvelope removes a metadata envelope and decrements all associated blob refs.
func (b *BoltDB) DeleteEnvelope(_ context.Context, protocol, kind, key string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket == nil {
			return nil
		}

		refsBucket := tx.Bucket(bucketEnvelopeBlobRefs)
		blobsBucket := tx.Bucket(bucketBlobsByHash)

		compoundKey := makeEnvelopeKey(protocol, kind, key)

		// Get refs for this key and decrement all
		if refsBucket != nil && blobsBucket != nil {
			refs := b.getEnvelopeBlobRefs(refsBucket, compoundKey)
			for _, hash := range refs {
				if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
					b.logger.Warn("failed to decrement blob ref during envelope delete",
						"protocol", protocol,
						"kind", kind,
						"key", key,
						"hash", hash,
						"error", err)
				}
			}
			if err := refsBucket.Delete(compoundKey); err != nil {
				return fmt.Errorf("deleting refs: %w", err)
			}
		}

		// Remove expiry index
		if err := b.removeEnvelopeExpiryIndex(tx, protocol, kind, key); err != nil {
			return err
		}

		return envBucket.Delete(compoundKey)
	})
}

// ListEnvelopeKeys returns all keys for a protocol/kind combination.
func (b *BoltDB) ListEnvelopeKeys(_ context.Context, protocol, kind string) ([]string, error) {
	var keys []string
	prefix := makeEnvelopeKey(protocol, kind, "")

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopes)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
			_, _, keyPart := parseEnvelopeKey(k)
			keys = append(keys, keyPart)
		}
		return nil
	})
	return keys, err
}

// UpdateEnvelope performs read-modify-write in a single transaction.
// The callback receives the current envelope (or nil if not found).
// If the callback returns a non-nil envelope, it is stored.
// If the callback returns nil, the entry is deleted.
func (b *BoltDB) UpdateEnvelope(ctx context.Context, protocol, kind, key string, fn func(*MetadataEnvelope) (*MetadataEnvelope, error)) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket == nil {
			return fmt.Errorf("envelopes bucket not found")
		}

		refsBucket := tx.Bucket(bucketEnvelopeBlobRefs)
		if refsBucket == nil {
			return fmt.Errorf("envelope_blob_refs bucket not found")
		}

		blobsBucket := tx.Bucket(bucketBlobsByHash)
		if blobsBucket == nil {
			return fmt.Errorf("blobs_by_hash bucket not found")
		}

		compoundKey := makeEnvelopeKey(protocol, kind, key)

		// Read existing envelope
		var existing *MetadataEnvelope
		val := envBucket.Get(compoundKey)
		if val != nil {
			existing = &MetadataEnvelope{}
			if err := proto.Unmarshal(val, existing); err != nil {
				return fmt.Errorf("unmarshaling existing envelope: %w", err)
			}
		}

		// Get old refs
		oldRefs := b.getEnvelopeBlobRefs(refsBucket, compoundKey)

		// Apply modification
		newEnv, err := fn(existing)
		if err != nil {
			return err
		}

		// If callback returns nil, delete the entry
		if newEnv == nil {
			// Decrement all old refs
			for _, hash := range oldRefs {
				if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
					b.logger.Warn("failed to decrement blob ref during update delete",
						"hash", hash, "error", err)
				}
			}
			if err := refsBucket.Delete(compoundKey); err != nil {
				return fmt.Errorf("deleting refs: %w", err)
			}
			if err := b.removeEnvelopeExpiryIndex(tx, protocol, kind, key); err != nil {
				return err
			}
			return envBucket.Delete(compoundKey)
		}

		// Validate and canonicalize
		if err := ValidateEnvelope(newEnv); err != nil {
			return fmt.Errorf("validating envelope: %w", err)
		}
		newEnv.BlobRefs = CanonicalizeRefs(newEnv.BlobRefs)

		// Compute ref diff
		added, removed := DiffRefs(oldRefs, newEnv.BlobRefs)

		// Update blob refcounts
		for _, hash := range added {
			if err := b.incrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("incrementing ref for %s: %w", hash, err)
			}
		}
		for _, hash := range removed {
			if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
				return fmt.Errorf("decrementing ref for %s: %w", hash, err)
			}
		}

		// Store new refs
		refsData, err := json.Marshal(newEnv.BlobRefs)
		if err != nil {
			return fmt.Errorf("marshaling refs: %w", err)
		}
		if err := refsBucket.Put(compoundKey, refsData); err != nil {
			return fmt.Errorf("putting refs: %w", err)
		}

		// Marshal and store envelope
		data, err := proto.Marshal(newEnv)
		if err != nil {
			return fmt.Errorf("marshaling envelope: %w", err)
		}
		if err := envBucket.Put(compoundKey, data); err != nil {
			return fmt.Errorf("putting envelope: %w", err)
		}

		// Update expiry index
		return b.updateEnvelopeExpiryIndex(tx, protocol, kind, key, newEnv.ExpiresAtUnixMs)
	})
}

// GetEnvelopeBlobRefs returns the blob refs for an envelope key.
func (b *BoltDB) GetEnvelopeBlobRefs(_ context.Context, protocol, kind, key string) ([]string, error) {
	var refs []string
	err := b.db.View(func(tx *bbolt.Tx) error {
		refsBucket := tx.Bucket(bucketEnvelopeBlobRefs)
		if refsBucket == nil {
			return nil
		}
		compoundKey := makeEnvelopeKey(protocol, kind, key)
		refs = b.getEnvelopeBlobRefs(refsBucket, compoundKey)
		return nil
	})
	return refs, err
}

// getEnvelopeBlobRefs retrieves blob refs from the refs bucket.
func (b *BoltDB) getEnvelopeBlobRefs(refsBucket *bbolt.Bucket, compoundKey []byte) []string {
	val := refsBucket.Get(compoundKey)
	if val == nil {
		return nil
	}
	var refs []string
	if err := json.Unmarshal(val, &refs); err != nil {
		return nil
	}
	return refs
}

// updateEnvelopeExpiryIndex updates the envelope expiry forward+reverse indexes.
func (b *BoltDB) updateEnvelopeExpiryIndex(tx *bbolt.Tx, protocol, kind, key string, expiresAtUnixMs int64) error {
	expiryBucket := tx.Bucket(bucketEnvelopeByExpiry)
	if expiryBucket == nil {
		return nil
	}

	reverseIndexBucket := tx.Bucket(bucketEnvelopeExpiryByKey)
	if reverseIndexBucket == nil {
		return nil
	}

	compoundKey := makeEnvelopeKey(protocol, kind, key)

	// Delete old index entries via reverse index lookup (O(1))
	if tsBytes := reverseIndexBucket.Get(compoundKey); tsBytes != nil {
		oldExpiresAt := decodeTimestamp(tsBytes)
		expiryKey := makeEnvelopeExpiryKey(oldExpiresAt, protocol, kind, key)
		if err := expiryBucket.Delete(expiryKey); err != nil {
			return fmt.Errorf("deleting old expiry index: %w", err)
		}
		if err := reverseIndexBucket.Delete(compoundKey); err != nil {
			return fmt.Errorf("deleting reverse index: %w", err)
		}
	}

	// If new expiry is set, write new index entries
	if expiresAtUnixMs > 0 {
		expiresAt := time.UnixMilli(expiresAtUnixMs)
		expiryKey := makeEnvelopeExpiryKey(expiresAt, protocol, kind, key)
		if err := expiryBucket.Put(expiryKey, compoundKey); err != nil {
			return fmt.Errorf("putting expiry index: %w", err)
		}
		if err := reverseIndexBucket.Put(compoundKey, encodeTimestamp(expiresAt)); err != nil {
			return fmt.Errorf("putting expiry reverse index: %w", err)
		}
	}

	return nil
}

// removeEnvelopeExpiryIndex removes envelope expiry index entries.
func (b *BoltDB) removeEnvelopeExpiryIndex(tx *bbolt.Tx, protocol, kind, key string) error {
	return b.updateEnvelopeExpiryIndex(tx, protocol, kind, key, 0)
}

// GetExpiredEnvelopes returns expired envelope entries for reaping.
func (b *BoltDB) GetExpiredEnvelopes(_ context.Context, before time.Time, limit int) ([]EnvelopeExpiryEntry, error) {
	var entries []EnvelopeExpiryEntry
	beforeBytes := encodeTimestamp(before)

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopeByExpiry)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil && len(entries) < limit; k, v = cursor.Next() {
			// Stop if we've passed the cutoff time
			if bytes.Compare(k[:8], beforeBytes) >= 0 {
				break
			}

			expiresAt, protocol, kind, key := parseEnvelopeExpiryKey(k)
			_ = v // value is the compound key, we already parsed it from k

			entries = append(entries, EnvelopeExpiryEntry{
				Protocol:  protocol,
				Kind:      kind,
				Key:       key,
				ExpiresAt: expiresAt,
			})
		}
		return nil
	})
	return entries, err
}

// DeleteExpiredEnvelopes batch-deletes expired envelopes in a single transaction.
func (b *BoltDB) DeleteExpiredEnvelopes(ctx context.Context, entries []EnvelopeExpiryEntry) error {
	if len(entries) == 0 {
		return nil
	}

	return b.db.Update(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		refsBucket := tx.Bucket(bucketEnvelopeBlobRefs)
		blobsBucket := tx.Bucket(bucketBlobsByHash)
		expiryBucket := tx.Bucket(bucketEnvelopeByExpiry)
		reverseIndexBucket := tx.Bucket(bucketEnvelopeExpiryByKey)

		for _, entry := range entries {
			compoundKey := makeEnvelopeKey(entry.Protocol, entry.Kind, entry.Key)
			expectedExpiry := encodeTimestamp(entry.ExpiresAt)

			// Expired entries are selected in a separate read transaction. A request
			// may refresh the same envelope before this delete transaction starts, so
			// only delete when the current reverse index still matches the snapshot.
			if reverseIndexBucket == nil || !bytes.Equal(reverseIndexBucket.Get(compoundKey), expectedExpiry) {
				continue
			}

			// Decrement blob refs
			if refsBucket != nil && blobsBucket != nil {
				refs := b.getEnvelopeBlobRefs(refsBucket, compoundKey)
				for _, hash := range refs {
					if err := b.decrementBlobRefInTx(blobsBucket, hash); err != nil {
						b.logger.Warn("failed to decrement blob ref during expiry delete",
							"protocol", entry.Protocol,
							"kind", entry.Kind,
							"key", entry.Key,
							"hash", hash,
							"error", err)
					}
				}
				_ = refsBucket.Delete(compoundKey)
			}

			// Delete expiry indexes
			if expiryBucket != nil {
				expiryKey := makeEnvelopeExpiryKey(entry.ExpiresAt, entry.Protocol, entry.Kind, entry.Key)
				_ = expiryBucket.Delete(expiryKey)
			}
			if reverseIndexBucket != nil {
				_ = reverseIndexBucket.Delete(compoundKey)
			}

			// Delete envelope
			if envBucket != nil {
				_ = envBucket.Delete(compoundKey)
			}
		}
		return nil
	})
}
