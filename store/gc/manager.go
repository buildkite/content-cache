// Package gc provides garbage collection for the content cache.
package gc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
)

// Config configures the GC manager.
type Config struct {
	Interval         time.Duration // How often to run (default: 1h)
	StartupDelay     time.Duration // Delay before first run (default: 5m)
	BatchSize        int           // Max items to process per run (default: 1000)
	CompactInterval  time.Duration // How often to compact bbolt (default: 24h)
	CompactThreshold float64       // Compact if free pages > threshold (default: 0.3)

	// BlobRetentionTTL is the minimum time a blob is kept after its last
	// access before GC may delete it, even when RefCount == 0.
	// Zero disables the floor and unreferenced blobs are deleted immediately.
	BlobRetentionTTL time.Duration
}

// DefaultConfig returns the default GC configuration.
func DefaultConfig() Config {
	return Config{
		Interval:         1 * time.Hour,
		StartupDelay:     5 * time.Minute,
		BatchSize:        1000,
		CompactInterval:  24 * time.Hour,
		CompactThreshold: 0.3,
	}
}

// Result contains the results of a GC run.
type Result struct {
	StartedAt                time.Time     `json:"started_at"`
	Duration                 time.Duration `json:"duration"`
	UnreferencedBlobsDeleted int           `json:"unreferenced_blobs_deleted"`
	OrphanBlobsDeleted       int           `json:"orphan_blobs_deleted"`
	ExpiredMetaDeleted       int           `json:"expired_meta_deleted"`
	BytesReclaimed           int64         `json:"bytes_reclaimed"`
	Errors                   []string      `json:"errors,omitempty"`
}

// BlobDeleteHook is called after a blob is successfully deleted from both the
// backend and MetaDB. hash is the blob's hex hash string; size is its byte count.
// Used to notify the S3-FIFO manager so it can clean up queue state.
type BlobDeleteHook func(ctx context.Context, hash string, size int64)

// Option configures a GC Manager.
type Option func(*Manager)

// WithBlobDeleteHook sets a hook that is called after each blob deletion.
func WithBlobDeleteHook(fn BlobDeleteHook) Option {
	return func(m *Manager) { m.blobDeleteHook = fn }
}

// Manager manages garbage collection for the content cache.
type Manager struct {
	db             metadb.MetaDB
	backend        backend.Backend
	config         Config
	metrics        *Metrics
	logger         *slog.Logger
	blobDeleteHook BlobDeleteHook

	stopCh  chan struct{}
	doneCh  chan struct{}
	mu      sync.Mutex
	running bool
	lastRun *Result
}

// New creates a new GC manager.
func New(db metadb.MetaDB, backend backend.Backend, config Config, metrics *Metrics, logger *slog.Logger, opts ...Option) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		db:      db,
		backend: backend,
		config:  config,
		metrics: metrics,
		logger:  logger,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start starts the background GC goroutine.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	go m.run(ctx)
}

// Stop gracefully stops the GC manager.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	close(m.stopCh)

	select {
	case <-m.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunNow triggers an immediate GC run.
func (m *Manager) RunNow(ctx context.Context) (*Result, error) {
	result := m.runGC(ctx)
	return result, nil
}

// Status returns the last GC run result.
func (m *Manager) Status() *Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRun
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.doneCh)

	m.logger.Info("gc manager starting",
		"interval", m.config.Interval,
		"startup_delay", m.config.StartupDelay,
	)

	// Wait for startup delay
	select {
	case <-time.After(m.config.StartupDelay):
	case <-m.stopCh:
		m.logger.Info("gc manager stopped during startup delay")
		m.setRunning(false)
		return
	case <-ctx.Done():
		m.logger.Info("gc manager context cancelled during startup delay")
		m.setRunning(false)
		return
	}

	// Run initial GC
	m.runGC(ctx)

	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.runGC(ctx)
		case <-m.stopCh:
			m.logger.Info("gc manager stopped")
			m.setRunning(false)
			return
		case <-ctx.Done():
			m.logger.Info("gc manager context cancelled")
			m.setRunning(false)
			return
		}
	}
}

func (m *Manager) setRunning(running bool) {
	m.mu.Lock()
	m.running = running
	m.mu.Unlock()
}

func (m *Manager) runGC(ctx context.Context) *Result {
	result := &Result{
		StartedAt: time.Now(),
	}

	m.logger.Info("starting gc run")

	// Phase 1: Delete expired metadata
	m.phaseExpireMeta(ctx, result)

	// Phase 2: Delete unreferenced blobs (RefCount == 0)
	m.phaseDeleteUnreferenced(ctx, result)

	// Phase 3: Delete orphan blobs (on disk but not in index)
	m.phaseDeleteOrphans(ctx, result)

	result.Duration = time.Since(result.StartedAt)

	// Update last run
	m.mu.Lock()
	m.lastRun = result
	m.mu.Unlock()

	// Record metrics
	m.recordMetrics(ctx, result)

	m.logger.Info("gc run completed",
		"duration", result.Duration,
		"unreferenced_blobs_deleted", result.UnreferencedBlobsDeleted,
		"orphan_blobs_deleted", result.OrphanBlobsDeleted,
		"expired_meta_deleted", result.ExpiredMetaDeleted,
		"bytes_reclaimed", result.BytesReclaimed,
		"errors", len(result.Errors),
	)

	return result
}

func (m *Manager) recordMetrics(ctx context.Context, result *Result) {
	if m.metrics == nil {
		return
	}

	m.metrics.runsTotal.Add(ctx, 1)
	m.metrics.runDuration.Record(ctx, result.Duration.Seconds())
	m.metrics.unreferencedBlobsDeleted.Add(ctx, int64(result.UnreferencedBlobsDeleted))
	m.metrics.orphanBlobsDeleted.Add(ctx, int64(result.OrphanBlobsDeleted))
	m.metrics.expiredMetaDeleted.Add(ctx, int64(result.ExpiredMetaDeleted))
	m.metrics.bytesReclaimed.Add(ctx, result.BytesReclaimed)
	m.metrics.errorsTotal.Add(ctx, int64(len(result.Errors)))
	m.metrics.lastRunTimestamp.Record(ctx, float64(result.StartedAt.Unix()))

	if len(result.Errors) == 0 {
		m.metrics.lastRunSuccess.Record(ctx, 1)
	} else {
		m.metrics.lastRunSuccess.Record(ctx, 0)
	}
}
