package metadb

import (
	"context"
	"log/slog"
	"time"

	"github.com/buildkite/content-cache/telemetry"
)

// EnvelopeReaper runs periodic cleanup of expired envelope metadata.
// Unlike the legacy ExpiryReaper, this uses batch deletion in a single
// transaction for efficiency and processes the new envelope storage format.
type EnvelopeReaper struct {
	db          EnvelopeExpiryStore
	interval    time.Duration
	batchSize   int
	maxDuration time.Duration
	logger      *slog.Logger
	now         func() time.Time

	// Metrics (for monitoring)
	lastReapTime  time.Time
	lastReapCount int
	totalReaped   int64
}

// EnvelopeReaperOption configures an EnvelopeReaper.
type EnvelopeReaperOption func(*EnvelopeReaper)

// WithEnvelopeReaperInterval sets the cleanup interval.
func WithEnvelopeReaperInterval(d time.Duration) EnvelopeReaperOption {
	return func(r *EnvelopeReaper) {
		r.interval = d
	}
}

// WithEnvelopeReaperBatchSize sets the maximum entries to process per reap cycle.
func WithEnvelopeReaperBatchSize(n int) EnvelopeReaperOption {
	return func(r *EnvelopeReaper) {
		r.batchSize = n
	}
}

// WithEnvelopeReaperMaxDuration sets the maximum time per reap cycle.
// If the cycle takes longer than this, it will stop and continue next tick.
func WithEnvelopeReaperMaxDuration(d time.Duration) EnvelopeReaperOption {
	return func(r *EnvelopeReaper) {
		r.maxDuration = d
	}
}

// WithEnvelopeReaperLogger sets the logger for the reaper.
func WithEnvelopeReaperLogger(logger *slog.Logger) EnvelopeReaperOption {
	return func(r *EnvelopeReaper) {
		r.logger = logger
	}
}

// WithEnvelopeReaperNow sets the time function (for testing).
func WithEnvelopeReaperNow(now func() time.Time) EnvelopeReaperOption {
	return func(r *EnvelopeReaper) {
		r.now = now
	}
}

// NewEnvelopeReaper creates a new envelope expiry reaper with the given options.
// Defaults: interval=5m, batchSize=100, maxDuration=30s.
func NewEnvelopeReaper(db EnvelopeExpiryStore, opts ...EnvelopeReaperOption) *EnvelopeReaper {
	r := &EnvelopeReaper{
		db:          db,
		interval:    5 * time.Minute,
		batchSize:   100,
		maxDuration: 30 * time.Second,
		logger:      slog.Default(),
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run starts the reaper loop. It blocks until the context is cancelled.
func (r *EnvelopeReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.logger.Debug("envelope reaper started",
		"interval", r.interval,
		"batchSize", r.batchSize,
		"maxDuration", r.maxDuration)

	for {
		select {
		case <-ctx.Done():
			r.logger.Debug("envelope reaper stopped", "totalReaped", r.totalReaped)
			return
		case <-ticker.C:
			r.reapCycle(ctx)
		}
	}
}

// reapCycle runs multiple batches until done or maxDuration exceeded.
func (r *EnvelopeReaper) reapCycle(ctx context.Context) {
	start := r.now()
	deadline := start.Add(r.maxDuration)
	cycleTotal := 0

	for {
		if r.now().After(deadline) {
			r.logger.Debug("reap cycle hit max duration, will continue next tick",
				"deleted", cycleTotal,
				"duration", r.now().Sub(start))
			break
		}

		count, hasMore := r.reapBatch(ctx)
		cycleTotal += count

		if !hasMore {
			break
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	if cycleTotal > 0 {
		r.lastReapTime = r.now()
		r.lastReapCount = cycleTotal
		r.totalReaped += int64(cycleTotal)

		r.logger.Info("envelope reaper cycle complete",
			"deleted", cycleTotal,
			"duration", r.now().Sub(start),
			"totalReaped", r.totalReaped)
	}

	telemetry.RecordReaperCycle(ctx, "envelope", cycleTotal, time.Since(start))
}

// reapBatch processes a single batch of expired entries.
// Returns the count deleted and whether there are more entries to process.
func (r *EnvelopeReaper) reapBatch(ctx context.Context) (int, bool) {
	now := r.now()

	expired, err := r.db.GetExpiredEnvelopes(ctx, now, r.batchSize)
	if err != nil {
		r.logger.Error("failed to get expired envelopes", "error", err)
		return 0, false
	}

	if len(expired) == 0 {
		return 0, false
	}

	r.logger.Debug("reaping expired envelopes", "count", len(expired))

	if err := r.db.DeleteExpiredEnvelopes(ctx, expired); err != nil {
		r.logger.Error("failed to batch delete expired envelopes",
			"error", err,
			"count", len(expired))
		return 0, false
	}

	hasMore := len(expired) == r.batchSize
	return len(expired), hasMore
}

// ReapNow runs a single reap cycle immediately.
// Useful for testing or manual cleanup.
func (r *EnvelopeReaper) ReapNow(ctx context.Context) int {
	start := r.now()
	total := 0

	for {
		count, hasMore := r.reapBatch(ctx)
		total += count
		if !hasMore {
			break
		}
	}

	if total > 0 {
		r.lastReapTime = r.now()
		r.lastReapCount = total
		r.totalReaped += int64(total)

		r.logger.Info("manual reap complete",
			"deleted", total,
			"duration", r.now().Sub(start))
	}

	return total
}

// Stats returns reaper statistics.
func (r *EnvelopeReaper) Stats() EnvelopeReaperStats {
	return EnvelopeReaperStats{
		LastReapTime:  r.lastReapTime,
		LastReapCount: r.lastReapCount,
		TotalReaped:   r.totalReaped,
		Interval:      r.interval,
		BatchSize:     r.batchSize,
	}
}

// EnvelopeReaperStats contains reaper statistics.
type EnvelopeReaperStats struct {
	LastReapTime  time.Time
	LastReapCount int
	TotalReaped   int64
	Interval      time.Duration
	BatchSize     int
}
