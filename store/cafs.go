package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/buildkite/content-cache/telemetry"
)

// MetadataTracker tracks blob metadata for expiration.
type MetadataTracker interface {
	// Create records metadata for a new blob.
	Create(ctx context.Context, hash contentcache.Hash, size int64) error
	// Touch updates the last access time for a blob.
	Touch(ctx context.Context, hash contentcache.Hash) error
	// Delete removes metadata for a blob.
	Delete(ctx context.Context, hash contentcache.Hash) error
}

// EvictionNotifier is notified of blob lifecycle events so that a cache
// eviction policy (e.g. S3-FIFO) can maintain its own state.
type EvictionNotifier interface {
	// Admit is called when a new blob is stored for the first time.
	Admit(ctx context.Context, hash string, size int64)
	// Remove is called when a blob is deleted externally (GC, explicit Delete).
	// size is the blob's byte count for accurate accounting; 0 if unknown.
	Remove(ctx context.Context, hash string, size int64)
}

// CAFS implements content-addressable file storage.
// Content is stored in a sharded directory structure based on hash.
type CAFS struct {
	backend          backend.Backend
	metadata         MetadataTracker  // Legacy tracker (keep for backwards compat)
	metaDB           metadb.MetaDB    // New MetaDB for blob tracking
	evictionNotifier EvictionNotifier // Optional S3-FIFO hook
}

// CAFSOption configures a CAFS instance.
type CAFSOption func(*CAFS)

// WithMetadataTracker sets a metadata tracker for expiration support.
func WithMetadataTracker(tracker MetadataTracker) CAFSOption {
	return func(c *CAFS) {
		c.metadata = tracker
	}
}

// WithMetaDB sets a MetaDB for blob tracking.
func WithMetaDB(db metadb.MetaDB) CAFSOption {
	return func(c *CAFS) {
		c.metaDB = db
	}
}

// WithEvictionNotifier wires an EvictionNotifier (e.g. the S3-FIFO Manager)
// into the CAFS so that admissions and external deletions are tracked.
func WithEvictionNotifier(n EvictionNotifier) CAFSOption {
	return func(c *CAFS) {
		c.evictionNotifier = n
	}
}

// NewCAFS creates a new content-addressable file store.
func NewCAFS(b backend.Backend, opts ...CAFSOption) *CAFS {
	c := &CAFS{backend: b}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Put stores content and returns its hash.
func (c *CAFS) Put(ctx context.Context, r io.Reader) (contentcache.Hash, error) {
	result, err := c.PutWithResult(ctx, r)
	if err != nil {
		return contentcache.Hash{}, err
	}
	return result.Hash, nil
}

// PutWithResult stores content and returns detailed information.
// Uses a temp file to avoid memory exhaustion for large content.
func (c *CAFS) PutWithResult(ctx context.Context, r io.Reader) (*PutResult, error) {
	// Create temp file for streaming content to avoid memory exhaustion
	tmpFile, err := os.CreateTemp("", "cafs-upload-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()
	defer func() { _ = tmpFile.Close() }()

	// Stream content to temp file while computing hash
	hr := contentcache.NewHashingReader(r)
	if _, err := io.Copy(tmpFile, hr); err != nil {
		return nil, fmt.Errorf("reading content: %w", err)
	}

	hash := hr.Sum()
	size := hr.BytesRead()
	key := contentcache.BlobStorageKey(hash)

	// Check if content already exists
	exists, err := c.backend.Exists(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("checking existence: %w", err)
	}

	blobRef := contentcache.NewBlobRef(hash).String()

	if exists {
		// Update access time (best effort, don't fail the operation)
		if c.metaDB != nil {
			newCount, err := c.metaDB.TouchBlob(ctx, blobRef)
			switch {
			case errors.Is(err, metadb.ErrNotFound):
				slog.Warn("blob exists on disk but missing from metadata", "hash", blobRef)
				telemetry.RecordBlobTouchMiss(ctx, telemetry.ProtocolFromContext(ctx))
			case err != nil:
				slog.Warn("blob touch failed", "hash", blobRef, "error", err)
			default:
				telemetry.RecordBlobTouch(ctx, telemetry.ProtocolFromContext(ctx), newCount)
			}
		} else if c.metadata != nil {
			_ = c.metadata.Touch(ctx, hash)
		}
		telemetry.RecordBlobWrite(ctx, telemetry.ProtocolFromContext(ctx), size, false)
		return &PutResult{
			Hash:   hash,
			Size:   size,
			Exists: true,
		}, nil
	}

	// Insert metadata BEFORE writing the file so that concurrent readers
	// never see a blob on disk without a corresponding DB row.
	if c.metaDB != nil {
		entry := &metadb.BlobEntry{
			Hash:       blobRef,
			Size:       size,
			CachedAt:   time.Now(),
			LastAccess: time.Now(),
			RefCount:   0,
		}
		if err := c.metaDB.PutBlob(ctx, entry); err != nil {
			return nil, fmt.Errorf("tracking blob metadata: %w", err)
		}
	} else if c.metadata != nil {
		_ = c.metadata.Create(ctx, hash, size)
	}

	// Seek to beginning of temp file for writing to backend
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	// Write content to backend
	if err := c.backend.Write(ctx, key, tmpFile); err != nil {
		// Clean up the metadata row if the file write fails
		if c.metaDB != nil {
			_ = c.metaDB.DeleteBlob(ctx, blobRef)
		}
		return nil, fmt.Errorf("writing content: %w", err)
	}

	if c.evictionNotifier != nil {
		c.evictionNotifier.Admit(ctx, blobRef, size)
	}

	telemetry.RecordBlobWrite(ctx, telemetry.ProtocolFromContext(ctx), size, true)
	return &PutResult{
		Hash:   hash,
		Size:   size,
		Exists: false,
	}, nil
}

// PutBytes is a convenience method for storing bytes.
func (c *CAFS) PutBytes(ctx context.Context, data []byte) (contentcache.Hash, error) {
	return c.Put(ctx, bytes.NewReader(data))
}

// Get retrieves content by its hash.
func (c *CAFS) Get(ctx context.Context, h contentcache.Hash) (io.ReadCloser, error) {
	key := contentcache.BlobStorageKey(h)
	rc, err := c.backend.Read(ctx, key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return nil, backend.ErrNotFound
		}
		return nil, fmt.Errorf("reading content: %w", err)
	}

	// Update access time asynchronously (best effort).
	// Capture protocol from the request context before spawning so the goroutine
	// can emit the correct label even after the request context is cancelled.
	if c.metaDB != nil {
		protocol := telemetry.ProtocolFromContext(ctx)
		touchCtx := telemetry.WithProtocolContext(context.Background(), protocol)
		blobRef := contentcache.NewBlobRef(h).String()
		go func() {
			newCount, err := c.metaDB.TouchBlob(touchCtx, blobRef)
			switch {
			case errors.Is(err, metadb.ErrNotFound):
				slog.Warn("blob exists on disk but missing from metadata", "hash", blobRef)
				telemetry.RecordBlobTouchMiss(touchCtx, protocol)
			case err != nil:
				slog.Warn("blob touch failed", "hash", blobRef, "error", err)
			default:
				telemetry.RecordBlobTouch(touchCtx, protocol, newCount)
			}
		}()
	} else if c.metadata != nil {
		go func() { _ = c.metadata.Touch(context.Background(), h) }()
	}

	return rc, nil
}

// GetBytes is a convenience method for retrieving content as bytes.
func (c *CAFS) GetBytes(ctx context.Context, h contentcache.Hash) ([]byte, error) {
	rc, err := c.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading content: %w", err)
	}
	return data, nil
}

// Has checks if content with the given hash exists.
func (c *CAFS) Has(ctx context.Context, h contentcache.Hash) (bool, error) {
	key := contentcache.BlobStorageKey(h)
	return c.backend.Exists(ctx, key)
}

// Delete removes content by its hash.
func (c *CAFS) Delete(ctx context.Context, h contentcache.Hash) error {
	key := contentcache.BlobStorageKey(h)
	if err := c.backend.Delete(ctx, key); err != nil {
		return err
	}

	// Look up size before deleting metadata so the eviction notifier can
	// accurately adjust its byte counters.
	blobRef := contentcache.NewBlobRef(h).String()
	var size int64
	if c.metaDB != nil {
		if entry, err := c.metaDB.GetBlob(ctx, blobRef); err == nil && entry != nil {
			size = entry.Size
		}
		_ = c.metaDB.DeleteBlob(ctx, blobRef)
	} else if c.metadata != nil {
		_ = c.metadata.Delete(ctx, h)
	}

	if c.evictionNotifier != nil {
		c.evictionNotifier.Remove(ctx, blobRef, size)
	}

	return nil
}

// Size returns the size of content with the given hash.
func (c *CAFS) Size(ctx context.Context, h contentcache.Hash) (int64, error) {
	key := contentcache.BlobStorageKey(h)

	// Try the SizeAwareBackend interface first
	if sb, ok := c.backend.(backend.SizeAwareBackend); ok {
		size, err := sb.Size(ctx, key)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return 0, backend.ErrNotFound
			}
			return 0, fmt.Errorf("getting size: %w", err)
		}
		return size, nil
	}

	// Fall back to reading the content
	rc, err := c.backend.Read(ctx, key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return 0, backend.ErrNotFound
		}
		return 0, fmt.Errorf("reading content: %w", err)
	}
	defer func() { _ = rc.Close() }()

	size, err := io.Copy(io.Discard, rc)
	if err != nil {
		return 0, fmt.Errorf("reading content for size: %w", err)
	}
	return size, nil
}

// List returns all hashes in the store.
func (c *CAFS) List(ctx context.Context) ([]contentcache.Hash, error) {
	keys, err := c.backend.List(ctx, contentcache.BlobKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("listing blobs: %w", err)
	}

	hashes := make([]contentcache.Hash, 0, len(keys))
	for _, key := range keys {
		h, err := contentcache.ParseBlobStorageKey(key)
		if err != nil {
			// Skip invalid keys (shouldn't happen in normal use)
			continue
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

// PutFramed stores content with headers and returns its hash.
func (c *CAFS) PutFramed(ctx context.Context, header *backend.BlobHeader, body io.Reader) (contentcache.Hash, error) {
	tmpFile, err := os.CreateTemp("", "cafs-upload-*")
	if err != nil {
		return contentcache.Hash{}, fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()
	defer func() { _ = tmpFile.Close() }()

	hr := contentcache.NewHashingReader(body)
	if _, err := io.Copy(tmpFile, hr); err != nil {
		return contentcache.Hash{}, fmt.Errorf("reading content: %w", err)
	}

	hash := hr.Sum()
	size := hr.BytesRead()
	key := contentcache.BlobStorageKey(hash)

	exists, err := c.backend.Exists(ctx, key)
	if err != nil {
		return contentcache.Hash{}, fmt.Errorf("checking existence: %w", err)
	}

	blobRef := contentcache.NewBlobRef(hash).String()

	if exists {
		if c.metaDB != nil {
			newCount, err := c.metaDB.TouchBlob(ctx, blobRef)
			switch {
			case errors.Is(err, metadb.ErrNotFound):
				slog.Warn("blob exists on disk but missing from metadata", "hash", blobRef)
				telemetry.RecordBlobTouchMiss(ctx, telemetry.ProtocolFromContext(ctx))
			case err != nil:
				slog.Warn("blob touch failed", "hash", blobRef, "error", err)
			default:
				telemetry.RecordBlobTouch(ctx, telemetry.ProtocolFromContext(ctx), newCount)
			}
		}
		telemetry.RecordBlobWrite(ctx, telemetry.ProtocolFromContext(ctx), size, false)
		return hash, nil
	}

	// Insert metadata BEFORE writing the file so that concurrent readers
	// never see a blob on disk without a corresponding DB row.
	if c.metaDB != nil {
		entry := &metadb.BlobEntry{
			Hash:       blobRef,
			Size:       size,
			CachedAt:   time.Now(),
			LastAccess: time.Now(),
			RefCount:   0,
		}
		if err := c.metaDB.PutBlob(ctx, entry); err != nil {
			return contentcache.Hash{}, fmt.Errorf("tracking blob metadata: %w", err)
		}
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return contentcache.Hash{}, fmt.Errorf("seeking temp file: %w", err)
	}

	header.ContentLength = size
	header.ContentHash = hash.String()
	if header.CachedAt == "" {
		header.CachedAt = time.Now().UTC().Format(time.RFC3339)
	}

	fb, ok := c.backend.(backend.FramedBackend)
	if !ok {
		return contentcache.Hash{}, fmt.Errorf("backend does not support framed writes")
	}

	if err := fb.WriteFramed(ctx, key, header, tmpFile); err != nil {
		if c.metaDB != nil {
			_ = c.metaDB.DeleteBlob(ctx, blobRef)
		}
		return contentcache.Hash{}, fmt.Errorf("writing framed content: %w", err)
	}

	if c.evictionNotifier != nil {
		c.evictionNotifier.Admit(ctx, blobRef, size)
	}

	telemetry.RecordBlobWrite(ctx, telemetry.ProtocolFromContext(ctx), size, true)
	return hash, nil
}

// GetFramed retrieves content with its headers.
func (c *CAFS) GetFramed(ctx context.Context, h contentcache.Hash) (*backend.BlobHeader, io.ReadCloser, error) {
	fb, ok := c.backend.(backend.FramedBackend)
	if !ok {
		return nil, nil, fmt.Errorf("backend does not support framed reads")
	}

	key := contentcache.BlobStorageKey(h)
	header, rc, err := fb.ReadFramed(ctx, key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return nil, nil, backend.ErrNotFound
		}
		return nil, nil, fmt.Errorf("reading framed content: %w", err)
	}

	if c.metaDB != nil {
		protocol := telemetry.ProtocolFromContext(ctx)
		touchCtx := telemetry.WithProtocolContext(context.Background(), protocol)
		blobRef := contentcache.NewBlobRef(h).String()
		go func() {
			newCount, err := c.metaDB.TouchBlob(touchCtx, blobRef)
			switch {
			case errors.Is(err, metadb.ErrNotFound):
				slog.Warn("blob exists on disk but missing from metadata", "hash", blobRef)
				telemetry.RecordBlobTouchMiss(touchCtx, protocol)
			case err != nil:
				slog.Warn("blob touch failed", "hash", blobRef, "error", err)
			default:
				telemetry.RecordBlobTouch(touchCtx, protocol, newCount)
			}
		}()
	}

	return header, rc, nil
}

// Compile-time interface checks
var (
	_ Store         = (*CAFS)(nil)
	_ ExtendedStore = (*CAFS)(nil)
)
