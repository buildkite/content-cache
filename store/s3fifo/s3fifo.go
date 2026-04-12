package s3fifo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/buildkite/content-cache/telemetry"
)

const (
	defaultSmallQueuePercent = 10
	defaultCheckInterval     = 30 * time.Second
	ghostFloor               = 128 // minimum ghost max entries when auto-sizing
)

// Config holds S3-FIFO eviction configuration.
type Config struct {
	// MaxSize is the maximum total size of cached blobs in bytes.
	MaxSize int64

	// SmallQueuePercent is the fraction of MaxSize reserved for the small
	// (probationary) queue. Default: 10.
	SmallQueuePercent int

	// GhostMaxEntries caps the ghost queue size.
	// 0 = auto: capped at the current main queue entry count (with a floor of ghostFloor).
	GhostMaxEntries int

	// CheckInterval is how often the background goroutine runs eviction.
	// Eviction also runs inline after each Admit call when over the limit.
	// Default: 30s.
	CheckInterval time.Duration

	// Logger for eviction events.
	Logger *slog.Logger
}

// Manager implements the S3-FIFO eviction algorithm on top of a bbolt-backed
// queue and an existing MetaDB / backend.
//
// Concurrency model:
//   - Admit and Remove are called from request-handling goroutines (via the
//     CAFS EvictionNotifier hook). They acquire m.mu briefly.
//   - A single background goroutine runs MaybeEvict on a ticker and on
//     inline signals sent by Admit.
//   - m.mu serialises all queue mutations and byte-counter updates.
type Manager struct {
	config  Config
	metaDB  metadb.MetaDB
	backend backend.Backend
	queues  Queues
	logger  *slog.Logger

	mu         sync.Mutex
	smallBytes int64
	mainBytes  int64
	smallLen   int
	mainLen    int
	ghostLen   int

	evictCh chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewManager creates and initialises a new S3-FIFO Manager.
// It recomputes byte totals from the persisted queue state so restarts are
// warm (no eviction penalty on startup).
func NewManager(queues Queues, mdb metadb.MetaDB, b backend.Backend, cfg Config) (*Manager, error) {
	if cfg.SmallQueuePercent <= 0 {
		cfg.SmallQueuePercent = defaultSmallQueuePercent
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = defaultCheckInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	m := &Manager{
		config:  cfg,
		metaDB:  mdb,
		backend: b,
		queues:  queues,
		logger:  cfg.Logger,
		evictCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	if err := m.recomputeState(context.Background()); err != nil {
		return nil, fmt.Errorf("s3fifo: recomputing state: %w", err)
	}

	return m, nil
}

// Start launches the background eviction goroutine. It must be called once.
func (m *Manager) Start(ctx context.Context) {
	go m.run(ctx)
}

// Stop signals the background goroutine to exit and waits for it to finish.
func (m *Manager) Stop() {
	close(m.stopCh)
	<-m.doneCh
}

// Admit records a newly cached blob and signals eviction if the cache is over
// the size limit. It implements the store.EvictionNotifier interface.
//
// Called from CAFS.PutWithResult / PutFramed after a new blob is written.
// Must NOT be called for blobs that already existed (Exists==true).
func (m *Manager) Admit(ctx context.Context, hash string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inGhost, err := m.queues.GhostContains(hash)
	if err != nil {
		m.logger.Warn("s3fifo: ghost check failed", "hash", hash, "error", err)
		// Fall through and admit to small.
	}

	if inGhost {
		if err := m.queues.AdmitGhostHit(hash); err != nil {
			m.logger.Warn("s3fifo: admit ghost hit failed", "hash", hash, "error", err)
			return
		}
		m.mainBytes += size
		m.mainLen++
		m.ghostLen--
		telemetry.RecordS3FIFOGhostHit(ctx)
		telemetry.RecordS3FIFOAdmission(ctx, QueueMain, "ghost_hit", size)
	} else {
		if err := m.queues.PushHead(QueueSmall, hash); err != nil {
			m.logger.Warn("s3fifo: push to small queue failed", "hash", hash, "error", err)
			return
		}
		m.smallBytes += size
		m.smallLen++
		telemetry.RecordS3FIFOAdmission(ctx, QueueSmall, "new", size)
	}

	// Signal the background eviction goroutine only when we are actually over
	// the size limit. Signalling unconditionally would wake the goroutine on
	// every Admit call — even well under capacity — causing unnecessary mutex
	// contention and bbolt I/O.
	if m.smallBytes+m.mainBytes > m.config.MaxSize {
		select {
		case m.evictCh <- struct{}{}:
		default:
		}
	}

	// Emit queue-state gauges on every admission so they are visible before
	// the first eviction run (which may be up to CheckInterval away).
	smallTarget := m.config.MaxSize * int64(m.config.SmallQueuePercent) / 100
	telemetry.UpdateS3FIFOQueueState(ctx,
		m.smallBytes, m.mainBytes,
		m.smallLen, m.mainLen, m.ghostLen,
		m.config.MaxSize, smallTarget,
	)
}

// Remove cleans up queue state when a blob is externally deleted (GC, CAFS.Delete).
// size is the blob size for accurate byte accounting; pass 0 if unknown
// (byte counters will be corrected on the next restart's recomputeBytes scan).
// It implements the store.EvictionNotifier interface.
func (m *Manager) Remove(ctx context.Context, hash string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if removed, err := m.queues.Remove(QueueSmall, hash); err != nil {
		m.logger.Warn("s3fifo: remove from small queue failed", "hash", hash, "error", err)
	} else if removed {
		m.smallBytes -= size
		if m.smallBytes < 0 {
			m.smallBytes = 0
		}
		m.smallLen--
		if m.smallLen < 0 {
			m.smallLen = 0
		}
	}

	if removed, err := m.queues.Remove(QueueMain, hash); err != nil {
		m.logger.Warn("s3fifo: remove from main queue failed", "hash", hash, "error", err)
	} else if removed {
		m.mainBytes -= size
		if m.mainBytes < 0 {
			m.mainBytes = 0
		}
		m.mainLen--
		if m.mainLen < 0 {
			m.mainLen = 0
		}
	}

	// Also purge from ghost (e.g. when GC deletes an evicted blob that was in ghost).
	if inGhost, _ := m.queues.GhostContains(hash); inGhost {
		if err := m.queues.GhostRemove(hash); err != nil {
			m.logger.Warn("s3fifo: ghost remove failed", "hash", hash, "error", err)
		} else {
			m.ghostLen--
			if m.ghostLen < 0 {
				m.ghostLen = 0
			}
		}
	}
}

// run is the background eviction goroutine.
func (m *Manager) run(ctx context.Context) {
	defer close(m.doneCh)

	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.evictCh:
			m.maybeEvict(ctx)
		case <-ticker.C:
			m.maybeEvict(ctx)
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// maybeEvict makes one logical eviction decision per call: evict or promote
// one item from the small queue (if over its target), or evict/second-chance
// one item from the main queue. If the cache is still over MaxSize after the
// decision, it signals evictCh so the background goroutine runs again on the
// next iteration, matching S3-FIFO's one-admission-one-eviction cadence.
func (m *Manager) maybeEvict(ctx context.Context) {
	start := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	smallTarget := m.config.MaxSize * int64(m.config.SmallQueuePercent) / 100

	if m.smallBytes+m.mainBytes > m.config.MaxSize {
		// smallActed is true when the small queue took any action (eviction or
		// promotion). Either way, we skip the main queue this pass — promotions
		// don't free bytes immediately but do make progress toward balance.
		smallActed := false

		// Prefer evicting from small when it exceeds its quota.
		if m.smallBytes > smallTarget && m.smallLen > 0 {
			skipped, err := m.evictFromSmall(ctx)
			if err != nil {
				m.logger.Warn("s3fifo: evict from small error", "error", err)
			} else if !skipped {
				smallActed = true
			}
		}

		// If small couldn't contribute (empty or all pinned), try main.
		if !smallActed && m.mainLen > 0 {
			skipped, err := m.evictFromMain(ctx)
			if err != nil {
				m.logger.Warn("s3fifo: evict from main error", "error", err)
			} else if !skipped {
				smallActed = true
			}
		}

		if !smallActed {
			m.logger.Warn("s3fifo: all eviction candidates pinned, allowing temporary overrun",
				"over_by", m.smallBytes+m.mainBytes-m.config.MaxSize,
			)
		}

		// If still over limit, schedule another eviction pass.
		if m.smallBytes+m.mainBytes > m.config.MaxSize {
			select {
			case m.evictCh <- struct{}{}:
			default:
			}
		}
	}

	// Update queue state gauges.
	telemetry.UpdateS3FIFOQueueState(ctx,
		m.smallBytes, m.mainBytes,
		m.smallLen, m.mainLen, m.ghostLen,
		m.config.MaxSize, smallTarget,
	)

	telemetry.RecordS3FIFOEvictionRun(ctx, time.Since(start))
}

// evictFromSmall pops the tail of the small queue and either:
//   - Skips (re-queues) if RefCount > 0 → returns skipped=true
//   - Promotes to main if AccessCount > 0
//   - Evicts and adds to ghost if AccessCount == 0 (one-hit wonder)
func (m *Manager) evictFromSmall(ctx context.Context) (skipped bool, err error) {
	hash, err := m.queues.PopTail(QueueSmall)
	if errors.Is(err, ErrQueueEmpty) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.smallLen--

	entry, err := m.metaDB.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			// Orphaned queue entry (blob deleted externally without Remove hook).
			// Drop it silently; byte counter will be corrected on restart.
			m.logger.Debug("s3fifo: orphaned small queue entry", "hash", hash)
			return false, nil
		}
		// Re-queue on transient errors to avoid losing the entry.
		_ = m.queues.PushHead(QueueSmall, hash)
		m.smallLen++
		return false, fmt.Errorf("get blob %s: %w", hash, err)
	}

	if entry.RefCount > 0 {
		// Pinned: must re-queue.
		if err := m.queues.PushHead(QueueSmall, hash); err != nil {
			return false, err
		}
		m.smallLen++
		telemetry.RecordS3FIFOPinnedSkip(ctx, QueueSmall)
		return true, nil
	}

	// Commit byte deduction now that we know we'll evict or promote.
	m.smallBytes -= entry.Size

	if entry.AccessCount > 0 {
		// Passed probation: promote to main queue. The frequency counter is
		// carried forward per the S3-FIFO paper — a blob that received N hits
		// in the small queue earns N second-chance passes in the main queue
		// before it can be evicted. No MetaDB write is needed here.
		if err := m.queues.PushHead(QueueMain, hash); err != nil {
			m.smallBytes += entry.Size
			return false, err
		}
		m.mainBytes += entry.Size
		m.mainLen++
		telemetry.RecordS3FIFOPromotion(ctx)
	} else {
		// One-hit wonder: evict and record in ghost set.
		if err := m.deleteFromBackend(ctx, hash); err != nil {
			m.smallBytes += entry.Size
			_ = m.queues.PushHead(QueueSmall, hash)
			m.smallLen++
			return false, err
		}
		if err := m.queues.GhostAdd(hash); err != nil {
			m.logger.Warn("s3fifo: ghost add failed", "hash", hash, "error", err)
		} else {
			m.ghostLen++
		}
		ghostMax := m.ghostMaxEntries()
		_ = m.queues.GhostTrimToMaxSize(ghostMax)
		if m.ghostLen > ghostMax {
			m.ghostLen = ghostMax
		}
		telemetry.RecordS3FIFOOneHitEviction(ctx, entry.Size)
		telemetry.RecordS3FIFOEviction(ctx, QueueSmall, entry.Size)
	}
	return false, nil
}

// evictFromMain pops the tail of the main queue and either:
//   - Skips (re-queues) if RefCount > 0 → returns skipped=true
//   - Reinserts with decremented AccessCount if AccessCount > 0 (second chance)
//   - Evicts if AccessCount == 0
func (m *Manager) evictFromMain(ctx context.Context) (skipped bool, err error) {
	hash, err := m.queues.PopTail(QueueMain)
	if errors.Is(err, ErrQueueEmpty) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.mainLen--

	entry, err := m.metaDB.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			m.logger.Debug("s3fifo: orphaned main queue entry", "hash", hash)
			return false, nil
		}
		_ = m.queues.PushHead(QueueMain, hash)
		m.mainLen++
		return false, fmt.Errorf("get blob %s: %w", hash, err)
	}

	if entry.RefCount > 0 {
		if err := m.queues.PushHead(QueueMain, hash); err != nil {
			return false, err
		}
		m.mainLen++
		telemetry.RecordS3FIFOPinnedSkip(ctx, QueueMain)
		return true, nil
	}

	if entry.AccessCount > 0 {
		// Second chance: decrement counter and reinsert at head.
		entry.AccessCount--
		if err := m.metaDB.PutBlob(ctx, entry); err != nil {
			_ = m.queues.PushHead(QueueMain, hash)
			m.mainLen++
			return false, fmt.Errorf("decrement access count for %s: %w", hash, err)
		}
		if err := m.queues.PushHead(QueueMain, hash); err != nil {
			return false, err
		}
		m.mainLen++
		telemetry.RecordS3FIFOSecondChance(ctx)
	} else {
		// Cold: evict.
		m.mainBytes -= entry.Size
		if err := m.deleteFromBackend(ctx, hash); err != nil {
			m.mainBytes += entry.Size
			_ = m.queues.PushHead(QueueMain, hash)
			return false, err
		}
		telemetry.RecordS3FIFOEviction(ctx, QueueMain, entry.Size)
	}
	return false, nil
}

// deleteFromBackend removes a blob from the filesystem backend and from MetaDB.
func (m *Manager) deleteFromBackend(ctx context.Context, hash string) error {
	ref, err := contentcache.ParseBlobRef(hash)
	if err != nil {
		return fmt.Errorf("parse hash %q: %w", hash, err)
	}
	key := contentcache.BlobStorageKey(ref.Hash)

	if err := m.backend.Delete(ctx, key); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("delete backend key %s: %w", key, err)
	}
	if err := m.metaDB.DeleteBlob(ctx, hash); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return fmt.Errorf("delete metadb entry %s: %w", hash, err)
	}
	return nil
}

// ghostMaxEntries returns the effective maximum ghost queue size.
// When GhostMaxEntries is 0 (auto), it mirrors the current main queue count
// with a minimum floor.
func (m *Manager) ghostMaxEntries() int {
	if m.config.GhostMaxEntries > 0 {
		return m.config.GhostMaxEntries
	}
	if m.mainLen < ghostFloor {
		return ghostFloor
	}
	return m.mainLen
}

// recomputeState iterates all queue entries and sums their sizes and counts
// from MetaDB. Called once at startup to restore in-memory counters from
// persisted state.
func (m *Manager) recomputeState(ctx context.Context) error {
	var small, main int64
	var smallCount, mainCount int

	for _, queue := range []string{QueueSmall, QueueMain} {
		err := m.queues.ForEach(queue, func(hash string) error {
			if queue == QueueSmall {
				smallCount++
			} else {
				mainCount++
			}
			entry, err := m.metaDB.GetBlob(ctx, hash)
			if err != nil {
				// Missing entry: queue is stale (blob was deleted without hook).
				// Skip silently; it will be cleaned up on the next eviction cycle.
				return nil
			}
			if queue == QueueSmall {
				small += entry.Size
			} else {
				main += entry.Size
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	ghostCount, err := m.queues.GhostLen()
	if err != nil {
		return err
	}

	m.smallBytes = small
	m.mainBytes = main
	m.smallLen = smallCount
	m.mainLen = mainCount
	m.ghostLen = ghostCount

	m.logger.Debug("s3fifo: recomputed state",
		"small_bytes", small,
		"main_bytes", main,
		"small_len", smallCount,
		"main_len", mainCount,
		"ghost_len", ghostCount,
	)
	return nil
}
