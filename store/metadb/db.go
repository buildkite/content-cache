package metadb

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when an entry does not exist.
var ErrNotFound = errors.New("metadb: not found")

// EnvelopeStore provides envelope-based metadata CRUD with blob reference tracking.
// Used by EnvelopeIndex for per-protocol/kind key storage.
type EnvelopeStore interface {
	PutEnvelope(ctx context.Context, protocol, kind, key string, env *MetadataEnvelope) error
	GetEnvelope(ctx context.Context, protocol, kind, key string) (*MetadataEnvelope, error)
	DeleteEnvelope(ctx context.Context, protocol, kind, key string) error
	ListEnvelopeKeys(ctx context.Context, protocol, kind string) ([]string, error)
	GetEnvelopeBlobRefs(ctx context.Context, protocol, kind, key string) ([]string, error)
	UpdateEnvelope(ctx context.Context, protocol, kind, key string, fn func(*MetadataEnvelope) (*MetadataEnvelope, error)) error
}

// EnvelopeExpiryStore handles expiry queries for the EnvelopeReaper.
type EnvelopeExpiryStore interface {
	GetExpiredEnvelopes(ctx context.Context, before time.Time, limit int) ([]EnvelopeExpiryEntry, error)
	DeleteExpiredEnvelopes(ctx context.Context, entries []EnvelopeExpiryEntry) error
}

// MetaExpiryStore is the narrow interface used by ExpiryReaper for legacy metadata cleanup.
type MetaExpiryStore interface {
	GetExpiredMeta(ctx context.Context, before time.Time, limit int) ([]ExpiryEntry, error)
	DeleteExpiredMetaWithRefs(ctx context.Context, entry ExpiryEntry) (bool, error)
}

// MetaDB provides metadata storage for the content cache.
type MetaDB interface {
	EnvelopeStore
	EnvelopeExpiryStore

	// Lifecycle
	Open(path string) error
	Close() error

	// Protocol metadata
	GetMeta(ctx context.Context, protocol, key string) ([]byte, error)
	PutMeta(ctx context.Context, protocol, key string, data []byte, ttl time.Duration) error
	DeleteMeta(ctx context.Context, protocol, key string) error
	DeleteMetaWithRefs(ctx context.Context, protocol, key string) error
	DeleteExpiredMetaWithRefs(ctx context.Context, entry ExpiryEntry) (bool, error)
	ListMeta(ctx context.Context, protocol string) ([]string, error)

	// Blob tracking
	GetBlob(ctx context.Context, hash string) (*BlobEntry, error)
	PutBlob(ctx context.Context, entry *BlobEntry) error
	DeleteBlob(ctx context.Context, hash string) error
	IncrementBlobRef(ctx context.Context, hash string) error
	DecrementBlobRef(ctx context.Context, hash string) error
	// TouchBlob updates the last access time and increments the access counter.
	// Returns the new access count (capped at 3 for S3-FIFO).
	// Returns (0, ErrNotFound) if the blob hash does not exist in the database.
	TouchBlob(ctx context.Context, hash string) (int, error)
	TotalBlobSize(ctx context.Context) (int64, error)

	// Eviction queries
	GetExpiredMeta(ctx context.Context, before time.Time, limit int) ([]ExpiryEntry, error)
	// GetUnreferencedBlobs returns blobs with RefCount == 0 whose last access
	// time is before the given cutoff. Pass a zero time to skip the cutoff filter.
	GetUnreferencedBlobs(ctx context.Context, before time.Time, limit int) ([]string, error)
}

// New creates a new MetaDB backed by bbolt.
func New() MetaDB {
	return NewBoltDB()
}
