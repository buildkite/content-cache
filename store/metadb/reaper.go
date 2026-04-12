package metadb

import (
	"context"
	"log/slog"
	"time"

	"github.com/buildkite/content-cache/telemetry"
)

// ExpiryReaper runs periodic cleanup of expired metadata.
type ExpiryReaper struct {
	db        MetaExpiryStore
	interval  time.Duration
	batchSize int
	logger    *slog.Logger
	now       func() time.Time
}

// ReaperOption configures an ExpiryReaper.
type ReaperOption func(*ExpiryReaper)

// WithReaperInterval sets the cleanup interval.
func WithReaperInterval(d time.Duration) ReaperOption {
	return func(r *ExpiryReaper) {
		r.interval = d
	}
}

// WithReaperBatchSize sets the maximum entries to process per reap cycle.
func WithReaperBatchSize(n int) ReaperOption {
	return func(r *ExpiryReaper) {
		r.batchSize = n
	}
}

// WithReaperLogger sets the logger for the reaper.
func WithReaperLogger(logger *slog.Logger) ReaperOption {
	return func(r *ExpiryReaper) {
		r.logger = logger
	}
}

// WithReaperNow sets the time function (for testing).
func WithReaperNow(now func() time.Time) ReaperOption {
	return func(r *ExpiryReaper) {
		r.now = now
	}
}

// NewExpiryReaper creates a new expiry reaper with the given options.
// Defaults: interval=5m, batchSize=100.
func NewExpiryReaper(db MetaExpiryStore, opts ...ReaperOption) *ExpiryReaper {
	r := &ExpiryReaper{
		db:        db,
		interval:  5 * time.Minute,
		batchSize: 100,
		logger:    slog.Default(),
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run starts the reaper loop. It blocks until the context is cancelled.
func (r *ExpiryReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.logger.Debug("expiry reaper started", "interval", r.interval, "batchSize", r.batchSize)

	for {
		select {
		case <-ctx.Done():
			r.logger.Debug("expiry reaper stopped")
			return
		case <-ticker.C:
			r.reapBatch(ctx)
		}
	}
}

// reapBatch processes a batch of expired entries.
func (r *ExpiryReaper) reapBatch(ctx context.Context) {
	start := time.Now()
	var deleted int
	defer func() {
		telemetry.RecordReaperCycle(ctx, "expiry", deleted, time.Since(start))
	}()

	expired, err := r.db.GetExpiredMeta(ctx, r.now(), r.batchSize)
	if err != nil {
		r.logger.Error("failed to get expired meta", "error", err)
		return
	}

	if len(expired) == 0 {
		return
	}

	r.logger.Debug("reaping expired entries", "count", len(expired))

	for _, entry := range expired {
		if err := r.db.DeleteMetaWithRefs(ctx, entry.Protocol, entry.Key); err != nil {
			r.logger.Warn("failed to delete expired entry",
				"protocol", entry.Protocol,
				"key", entry.Key,
				"error", err)
			continue
		}
		deleted++
	}

	r.logger.Info("expired entries reaped",
		"deleted", deleted,
		"total", len(expired))
}

// ReapNow runs a single reap cycle immediately.
// Useful for testing.
func (r *ExpiryReaper) ReapNow(ctx context.Context) {
	r.reapBatch(ctx)
}
