package gc

import (
	"context"
	"errors"
	"fmt"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
)

// phaseExpireMeta deletes expired metadata entries.
func (m *Manager) phaseExpireMeta(ctx context.Context, result *Result) {
	m.logger.Debug("phase: expire metadata")

	expired, err := m.db.GetExpiredMeta(ctx, time.Now(), m.config.BatchSize)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("get expired meta: %v", err))
		m.logger.Error("failed to get expired metadata", "error", err)
		return
	}

	for _, entry := range expired {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := m.db.DeleteMetaWithRefs(ctx, entry.Protocol, entry.Key); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("delete meta %s/%s: %v", entry.Protocol, entry.Key, err))
			m.logger.Error("failed to delete expired metadata",
				"protocol", entry.Protocol,
				"key", entry.Key,
				"error", err,
			)
			continue
		}

		result.ExpiredMetaDeleted++
		result.BytesReclaimed += entry.Size

		m.logger.Debug("deleted expired metadata",
			"protocol", entry.Protocol,
			"key", entry.Key,
			"expired_at", entry.ExpiresAt,
		)
	}
}

// phaseDeleteUnreferenced deletes blobs with RefCount == 0 that have not been
// accessed within the configured retention window.
func (m *Manager) phaseDeleteUnreferenced(ctx context.Context, result *Result) {
	m.logger.Debug("phase: delete unreferenced blobs")

	var before time.Time
	if m.config.BlobRetentionTTL > 0 {
		before = time.Now().Add(-m.config.BlobRetentionTTL)
	}

	hashes, err := m.db.GetUnreferencedBlobs(ctx, before, m.config.BatchSize)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("get unreferenced blobs: %v", err))
		m.logger.Error("failed to get unreferenced blobs", "error", err)
		return
	}

	for _, hash := range hashes {
		select {
		case <-ctx.Done():
			return
		default:
		}

		bytesReclaimed, err := m.deleteBlob(ctx, hash)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("delete unreferenced blob %s: %v", hash, err))
			m.logger.Error("failed to delete unreferenced blob", "hash", hash, "error", err)
			continue
		}

		result.UnreferencedBlobsDeleted++
		result.BytesReclaimed += bytesReclaimed

		m.logger.Debug("deleted unreferenced blob", "hash", hash, "size", bytesReclaimed)
	}
}

// phaseDeleteOrphans deletes blobs on disk but not in the index.
func (m *Manager) phaseDeleteOrphans(ctx context.Context, result *Result) {
	m.logger.Debug("phase: delete orphan blobs")

	keys, err := m.backend.List(ctx, "blobs/")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("list backend blobs: %v", err))
		m.logger.Error("failed to list backend blobs", "error", err)
		return
	}

	processed := 0
	for _, key := range keys {
		if processed >= m.config.BatchSize {
			break
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		h, err := contentcache.ParseBlobStorageKey(key)
		if err != nil {
			continue
		}
		// Try the canonical prefixed key first, then fall back to the legacy plain-hex
		// key for databases written before the blake3: prefix was introduced.
		hash := contentcache.NewBlobRef(h).String()
		_, err = m.db.GetBlob(ctx, hash)
		if errors.Is(err, metadb.ErrNotFound) {
			_, err = m.db.GetBlob(ctx, h.String())
		}
		if err == nil {
			continue
		}

		if !errors.Is(err, metadb.ErrNotFound) {
			result.Errors = append(result.Errors, fmt.Sprintf("check blob %s: %v", hash, err))
			continue
		}

		var size int64
		if sizeBackend, ok := m.backend.(interface {
			Size(context.Context, string) (int64, error)
		}); ok {
			size, _ = sizeBackend.Size(ctx, key)
		}

		if err := m.backend.Delete(ctx, key); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("delete orphan blob %s: %v", key, err))
			m.logger.Error("failed to delete orphan blob", "key", key, "error", err)
			continue
		}

		result.OrphanBlobsDeleted++
		result.BytesReclaimed += size
		processed++

		m.logger.Debug("deleted orphan blob", "key", key, "size", size)
	}
}

// deleteBlob deletes a blob from both the backend and metadata store.
func (m *Manager) deleteBlob(ctx context.Context, hash string) (int64, error) {
	entry, err := m.db.GetBlob(ctx, hash)
	if err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return 0, fmt.Errorf("get blob metadata: %w", err)
	}

	var size int64
	if entry != nil {
		size = entry.Size
	}

	ref, err := contentcache.ParseBlobRef(hash)
	if err != nil {
		return 0, fmt.Errorf("parse hash: %w", err)
	}

	key := contentcache.BlobStorageKey(ref.Hash)
	if err := m.backend.Delete(ctx, key); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return 0, fmt.Errorf("delete from backend: %w", err)
	}

	if err := m.db.DeleteBlob(ctx, hash); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return 0, fmt.Errorf("delete from metadb: %w", err)
	}

	// Always notify the hook, even when size==0 (blob was already absent from
	// MetaDB). Remove() guards against counter underflow, so passing 0 is safe.
	if m.blobDeleteHook != nil {
		m.blobDeleteHook(ctx, hash, size)
	}

	return size, nil
}
