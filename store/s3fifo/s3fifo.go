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
//   - Concurrent Admit calls share queueMu and use bbolt's batch API so their
//     queue writes can commit together. Remove and eviction take queueMu
//     exclusively while selecting or mutating queue membership. Eviction
//     releases it between candidates so uploads can proceed during long scans.
//   - A single background goroutine runs MaybeEvict on a ticker and on
//     inline signals sent by Admit.
//   - m.mu protects only in-memory byte and length counters. Filesystem and
//     MetaDB deletion must never run while either lock is held.
type Manager struct {
	config  Config
	metaDB  metadb.MetaDB
	backend backend.Backend
	queues  Queues
	logger  *slog.Logger

	queueMu    sync.RWMutex
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
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("s3fifo: MaxSize must be greater than zero")
	}
	if cfg.SmallQueuePercent <= 0 {
		cfg.SmallQueuePercent = defaultSmallQueuePercent
	}
	if cfg.SmallQueuePercent > 100 {
		return nil, fmt.Errorf("s3fifo: SmallQueuePercent must be between 1 and 100")
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
	smallTarget := m.config.MaxSize * int64(m.config.SmallQueuePercent) / 100
	telemetry.UpdateS3FIFOQueueState(context.Background(),
		m.smallBytes, m.mainBytes,
		m.smallLen, m.mainLen, m.ghostLen,
		m.config.MaxSize, smallTarget,
	)
	if m.smallBytes+m.mainBytes > m.config.MaxSize {
		m.evictCh <- struct{}{}
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
	m.queueMu.RLock()

	inGhost, err := m.queues.GhostContains(hash)
	if err != nil {
		m.logger.Warn("s3fifo: ghost check failed", "hash", hash, "error", err)
		telemetry.RecordS3FIFOEvictionError(ctx, "ghost", "queue_read")
		// Fall through and admit to small.
	}

	queue := QueueSmall
	reason := "new"
	admitted := false
	if inGhost {
		ghostAdmitted, err := m.queues.AdmitGhostHit(hash)
		if err != nil {
			m.queueMu.RUnlock()
			m.logger.Warn("s3fifo: admit ghost hit failed", "hash", hash, "error", err)
			telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "queue_write")
			return
		}
		if ghostAdmitted {
			queue = QueueMain
			reason = "ghost_hit"
			admitted = true
		}
	} else {
		replaced, err := m.queues.PushHead(QueueSmall, hash)
		if err != nil {
			m.queueMu.RUnlock()
			m.logger.Warn("s3fifo: push to small queue failed", "hash", hash, "error", err)
			telemetry.RecordS3FIFOEvictionError(ctx, QueueSmall, "queue_write")
			return
		}
		admitted = !replaced
	}

	m.mu.Lock()
	if admitted {
		if queue == QueueMain {
			m.mainBytes += size
			m.mainLen++
			m.ghostLen--
			if m.ghostLen < 0 {
				m.ghostLen = 0
			}
		} else {
			m.smallBytes += size
			m.smallLen++
		}
	}
	overLimit := m.smallBytes+m.mainBytes > m.config.MaxSize
	smallBytes, mainBytes := m.smallBytes, m.mainBytes
	smallLen, mainLen, ghostLen := m.smallLen, m.mainLen, m.ghostLen
	m.mu.Unlock()
	m.queueMu.RUnlock()

	if !admitted {
		return
	}
	if queue == QueueMain {
		telemetry.RecordS3FIFOGhostHit(ctx)
	}
	telemetry.RecordS3FIFOAdmission(ctx, queue, reason, size)

	// Signal the background eviction goroutine only when we are actually over
	// the size limit. Signalling unconditionally would wake the goroutine on
	// every Admit call — even well under capacity — causing unnecessary mutex
	// contention and bbolt I/O.
	if overLimit {
		select {
		case m.evictCh <- struct{}{}:
		default:
		}
	}

	// Emit queue-state gauges on every admission so they are visible before
	// the first eviction run (which may be up to CheckInterval away).
	smallTarget := m.config.MaxSize * int64(m.config.SmallQueuePercent) / 100
	telemetry.UpdateS3FIFOQueueState(ctx,
		smallBytes, mainBytes,
		smallLen, mainLen, ghostLen,
		m.config.MaxSize, smallTarget,
	)
}

// Remove cleans up queue state when a blob is externally deleted (GC, CAFS.Delete).
// size is the blob size for accurate byte accounting; pass 0 if unknown
// (byte counters will be corrected on the next restart's recomputeBytes scan).
// It implements the store.EvictionNotifier interface.
func (m *Manager) Remove(ctx context.Context, hash string, size int64) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()

	if removed, err := m.queues.Remove(QueueSmall, hash); err != nil {
		m.logger.Warn("s3fifo: remove from small queue failed", "hash", hash, "error", err)
		telemetry.RecordS3FIFOEvictionError(ctx, QueueSmall, "queue_remove")
	} else if removed {
		m.mu.Lock()
		m.smallBytes -= size
		if m.smallBytes < 0 {
			m.smallBytes = 0
		}
		m.smallLen--
		if m.smallLen < 0 {
			m.smallLen = 0
		}
		m.mu.Unlock()
	}

	if removed, err := m.queues.Remove(QueueMain, hash); err != nil {
		m.logger.Warn("s3fifo: remove from main queue failed", "hash", hash, "error", err)
		telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "queue_remove")
	} else if removed {
		m.mu.Lock()
		m.mainBytes -= size
		if m.mainBytes < 0 {
			m.mainBytes = 0
		}
		m.mainLen--
		if m.mainLen < 0 {
			m.mainLen = 0
		}
		m.mu.Unlock()
	}

	// Also purge from ghost (e.g. when GC deletes an evicted blob that was in ghost).
	if inGhost, _ := m.queues.GhostContains(hash); inGhost {
		if err := m.queues.GhostRemove(hash); err != nil {
			m.logger.Warn("s3fifo: ghost remove failed", "hash", hash, "error", err)
			telemetry.RecordS3FIFOEvictionError(ctx, "ghost", "queue_remove")
		} else {
			m.mu.Lock()
			m.ghostLen--
			if m.ghostLen < 0 {
				m.ghostLen = 0
			}
			m.mu.Unlock()
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
	smallTarget := m.config.MaxSize * int64(m.config.SmallQueuePercent) / 100
	acted := false
	var decisionErr error
	var pending *pendingEviction

	state := m.snapshotState()
	if state.smallBytes+state.mainBytes > m.config.MaxSize {
		acted, pending, decisionErr = m.scanEvictionQueue(ctx, QueueSmall, state.smallLen, smallTarget)
		if decisionErr != nil && ctx.Err() == nil {
			m.logger.Warn("s3fifo: evict from small error", "error", decisionErr)
		}

		// If small couldn't contribute (empty or all pinned), try main.
		state = m.snapshotState()
		if !acted && decisionErr == nil {
			acted, pending, decisionErr = m.scanEvictionQueue(ctx, QueueMain, state.mainLen, smallTarget)
			if decisionErr != nil && ctx.Err() == nil {
				m.logger.Warn("s3fifo: evict from main error", "error", decisionErr)
			}
		}
	}

	if pending != nil {
		if err := m.deleteFromBackend(ctx, pending.queue, pending.hash); err != nil {
			m.restorePendingEviction(ctx, pending)
			decisionErr = err
			acted = false
			m.logger.Warn("s3fifo: delete eviction candidate error", "queue", pending.queue, "hash", pending.hash, "error", err)
		} else {
			m.finishPendingEviction(ctx, pending)
		}
	}

	state = m.snapshotState()
	if !acted && decisionErr == nil && state.smallBytes+state.mainBytes > m.config.MaxSize {
		m.logger.Warn("s3fifo: all eviction candidates pinned, allowing temporary overrun",
			"over_by", state.smallBytes+state.mainBytes-m.config.MaxSize,
		)
		telemetry.RecordS3FIFOEvictionBlocked(ctx, "all_candidates_pinned")
	}

	// If still over limit, schedule another eviction pass only after real
	// progress. When every candidate is pinned, retrying immediately just
	// spins; the ticker or a later admission will retry eviction.
	if acted && state.smallBytes+state.mainBytes > m.config.MaxSize {
		select {
		case m.evictCh <- struct{}{}:
		default:
		}
	}

	// Update queue state gauges.
	telemetry.UpdateS3FIFOQueueState(ctx,
		state.smallBytes, state.mainBytes,
		state.smallLen, state.mainLen, state.ghostLen,
		m.config.MaxSize, smallTarget,
	)

	telemetry.RecordS3FIFOEvictionRun(ctx, time.Since(start))
}

// scanEvictionQueue makes at most one eviction, promotion or second-chance
// decision. Pinned entries are rotated, with the scan bounded by the initial
// queue length so an all-pinned queue cannot cause an unbounded retry loop.
func (m *Manager) scanEvictionQueue(ctx context.Context, queue string, attempts int, smallTarget int64) (bool, *pendingEviction, error) {
	for range attempts {
		if err := ctx.Err(); err != nil {
			return false, nil, err
		}

		// Queue membership and counters must change atomically for a candidate,
		// but admissions and removals may proceed between candidates. Recheck
		// capacity under the lock before selecting another blob.
		m.queueMu.Lock()
		state := m.snapshotState()
		if state.smallBytes+state.mainBytes <= m.config.MaxSize ||
			(queue == QueueSmall && (state.smallBytes <= smallTarget || state.smallLen == 0)) ||
			(queue == QueueMain && state.mainLen == 0) {
			m.queueMu.Unlock()
			return false, nil, nil
		}

		var skipped bool
		var pending *pendingEviction
		var err error
		if queue == QueueSmall {
			skipped, pending, err = m.evictFromSmall(ctx)
		} else {
			skipped, pending, err = m.evictFromMain(ctx)
		}
		m.queueMu.Unlock()
		if err != nil || !skipped {
			return err == nil, pending, err
		}
	}
	return false, nil, nil
}

type queueState struct {
	smallBytes int64
	mainBytes  int64
	smallLen   int
	mainLen    int
	ghostLen   int
}

func (m *Manager) snapshotState() queueState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return queueState{
		smallBytes: m.smallBytes,
		mainBytes:  m.mainBytes,
		smallLen:   m.smallLen,
		mainLen:    m.mainLen,
		ghostLen:   m.ghostLen,
	}
}

type pendingEviction struct {
	queue      string
	hash       string
	size       int64
	addToGhost bool
}

// evictFromSmall pops the tail of the small queue and either:
//   - Skips (re-queues) if RefCount > 0 → returns skipped=true
//   - Promotes to main if AccessCount > 0
//   - Evicts and adds to ghost if AccessCount == 0 (one-hit wonder)
func (m *Manager) evictFromSmall(ctx context.Context) (skipped bool, pending *pendingEviction, err error) {
	hash, err := m.queues.PopTail(QueueSmall)
	if errors.Is(err, ErrQueueEmpty) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	m.mu.Lock()
	m.smallLen--
	m.mu.Unlock()

	entry, err := m.metaDB.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			// Orphaned queue entry (blob deleted externally without Remove hook).
			// Drop it silently; byte counter will be corrected on restart.
			m.logger.Debug("s3fifo: orphaned small queue entry", "hash", hash)
			telemetry.RecordS3FIFOOrphanedQueueEntry(ctx, QueueSmall)
			return false, nil, nil
		}
		// Re-queue on transient errors to avoid losing the entry.
		_, _ = m.queues.PushHead(QueueSmall, hash)
		m.mu.Lock()
		m.smallLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOEvictionError(ctx, QueueSmall, "metadb_get")
		return false, nil, fmt.Errorf("get blob %s: %w", hash, err)
	}

	if entry.RefCount > 0 {
		// Pinned: must re-queue.
		if _, err := m.queues.PushHead(QueueSmall, hash); err != nil {
			telemetry.RecordS3FIFOEvictionError(ctx, QueueSmall, "queue_write")
			return false, nil, err
		}
		m.mu.Lock()
		m.smallLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOPinnedSkip(ctx, QueueSmall)
		return true, nil, nil
	}

	// Commit byte deduction now that we know we'll evict or promote.
	m.mu.Lock()
	m.smallBytes -= entry.Size
	m.mu.Unlock()

	if entry.AccessCount > 0 {
		// Passed probation: promote to main queue. The frequency counter is
		// carried forward per the S3-FIFO paper — a blob that received N hits
		// in the small queue earns N second-chance passes in the main queue
		// before it can be evicted. No MetaDB write is needed here.
		if _, err := m.queues.PushHead(QueueMain, hash); err != nil {
			_, _ = m.queues.PushHead(QueueSmall, hash)
			m.mu.Lock()
			m.smallBytes += entry.Size
			m.smallLen++
			m.mu.Unlock()
			telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "queue_write")
			return false, nil, err
		}
		m.mu.Lock()
		m.mainBytes += entry.Size
		m.mainLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOPromotion(ctx)
	} else {
		return false, &pendingEviction{queue: QueueSmall, hash: hash, size: entry.Size, addToGhost: true}, nil
	}
	return false, nil, nil
}

// evictFromMain pops the tail of the main queue and either:
//   - Skips (re-queues) if RefCount > 0 → returns skipped=true
//   - Reinserts with decremented AccessCount if AccessCount > 0 (second chance)
//   - Evicts if AccessCount == 0
func (m *Manager) evictFromMain(ctx context.Context) (skipped bool, pending *pendingEviction, err error) {
	hash, err := m.queues.PopTail(QueueMain)
	if errors.Is(err, ErrQueueEmpty) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	m.mu.Lock()
	m.mainLen--
	m.mu.Unlock()

	entry, err := m.metaDB.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			m.logger.Debug("s3fifo: orphaned main queue entry", "hash", hash)
			telemetry.RecordS3FIFOOrphanedQueueEntry(ctx, QueueMain)
			return false, nil, nil
		}
		_, _ = m.queues.PushHead(QueueMain, hash)
		m.mu.Lock()
		m.mainLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "metadb_get")
		return false, nil, fmt.Errorf("get blob %s: %w", hash, err)
	}

	if entry.RefCount > 0 {
		if _, err := m.queues.PushHead(QueueMain, hash); err != nil {
			telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "queue_write")
			return false, nil, err
		}
		m.mu.Lock()
		m.mainLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOPinnedSkip(ctx, QueueMain)
		return true, nil, nil
	}

	if entry.AccessCount > 0 {
		// Second chance: decrement counter and reinsert at head.
		entry.AccessCount--
		if err := m.metaDB.PutBlob(ctx, entry); err != nil {
			_, _ = m.queues.PushHead(QueueMain, hash)
			m.mu.Lock()
			m.mainLen++
			m.mu.Unlock()
			telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "metadb_put")
			return false, nil, fmt.Errorf("decrement access count for %s: %w", hash, err)
		}
		if _, err := m.queues.PushHead(QueueMain, hash); err != nil {
			telemetry.RecordS3FIFOEvictionError(ctx, QueueMain, "queue_write")
			return false, nil, err
		}
		m.mu.Lock()
		m.mainLen++
		m.mu.Unlock()
		telemetry.RecordS3FIFOSecondChance(ctx)
	} else {
		// Cold: account for removal now and delete outside the queue lock.
		m.mu.Lock()
		m.mainBytes -= entry.Size
		m.mu.Unlock()
		return false, &pendingEviction{queue: QueueMain, hash: hash, size: entry.Size}, nil
	}
	return false, nil, nil
}

func (m *Manager) restorePendingEviction(ctx context.Context, pending *pendingEviction) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()

	replaced, err := m.queues.PushHead(pending.queue, pending.hash)
	if err != nil {
		telemetry.RecordS3FIFOEvictionError(ctx, pending.queue, "queue_restore")
		m.logger.Error("s3fifo: failed to restore eviction candidate", "queue", pending.queue, "hash", pending.hash, "error", err)
		return
	}
	if replaced {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if pending.queue == QueueSmall {
		m.smallBytes += pending.size
		m.smallLen++
	} else {
		m.mainBytes += pending.size
		m.mainLen++
	}
}

func (m *Manager) finishPendingEviction(ctx context.Context, pending *pendingEviction) {
	if pending.addToGhost {
		m.queueMu.Lock()
		if err := m.queues.GhostAdd(pending.hash); err != nil {
			m.logger.Warn("s3fifo: ghost add failed", "hash", pending.hash, "error", err)
			telemetry.RecordS3FIFOEvictionError(ctx, "ghost", "queue_write")
		} else {
			m.mu.Lock()
			m.ghostLen++
			m.mu.Unlock()
		}
		ghostMax := m.ghostMaxEntries()
		if err := m.queues.GhostTrimToMaxSize(ghostMax); err != nil {
			telemetry.RecordS3FIFOEvictionError(ctx, "ghost", "queue_trim")
		}
		m.mu.Lock()
		if m.ghostLen > ghostMax {
			m.ghostLen = ghostMax
		}
		m.mu.Unlock()
		m.queueMu.Unlock()
		telemetry.RecordS3FIFOOneHitEviction(ctx, pending.size)
	}
	telemetry.RecordS3FIFOEviction(ctx, pending.queue, pending.size)
}

// deleteFromBackend removes a blob from the filesystem backend and from MetaDB.
func (m *Manager) deleteFromBackend(ctx context.Context, queue, hash string) error {
	ref, err := contentcache.ParseBlobRef(hash)
	if err != nil {
		telemetry.RecordS3FIFOEvictionError(ctx, queue, "parse_hash")
		return fmt.Errorf("parse hash %q: %w", hash, err)
	}
	key := contentcache.BlobStorageKey(ref.Hash)

	if err := m.backend.Delete(ctx, key); err != nil && !errors.Is(err, backend.ErrNotFound) {
		telemetry.RecordS3FIFOEvictionError(ctx, queue, "backend_delete")
		return fmt.Errorf("delete backend key %s: %w", key, err)
	}
	if err := m.metaDB.DeleteBlob(ctx, hash); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		telemetry.RecordS3FIFOEvictionError(ctx, queue, "metadb_delete")
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
