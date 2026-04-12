// Package store provides content-addressable storage implementations.
package store

import (
	"context"
	"io"

	contentcache "github.com/buildkite/content-cache"
)

// Store provides content-addressable storage operations.
// Content is stored by its BLAKE3 hash, ensuring deduplication.
type Store interface {
	// Put stores content and returns its hash.
	// If the content already exists (same hash), this is a no-op.
	Put(ctx context.Context, r io.Reader) (contentcache.Hash, error)

	// Get retrieves content by its hash.
	// Returns ErrNotFound if the hash does not exist.
	// The caller must close the returned ReadCloser.
	Get(ctx context.Context, h contentcache.Hash) (io.ReadCloser, error)

	// Has checks if content with the given hash exists.
	Has(ctx context.Context, h contentcache.Hash) (bool, error)

	// Delete removes content by its hash.
	// Returns nil if the content does not exist (idempotent).
	Delete(ctx context.Context, h contentcache.Hash) error

	// Size returns the size of content with the given hash.
	// Returns ErrNotFound if the hash does not exist.
	Size(ctx context.Context, h contentcache.Hash) (int64, error)
}

// PutResult contains information about a Put operation.
type PutResult struct {
	Hash   contentcache.Hash
	Size   int64
	Exists bool // true if the content already existed
}

// ExtendedStore provides additional operations beyond the basic Store.
type ExtendedStore interface {
	Store

	// PutWithResult stores content and returns detailed information.
	PutWithResult(ctx context.Context, r io.Reader) (*PutResult, error)

	// List returns all hashes in the store.
	// This may be expensive for large stores.
	List(ctx context.Context) ([]contentcache.Hash, error)
}
