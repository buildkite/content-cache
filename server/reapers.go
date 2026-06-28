package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
)

// metadataReapers owns the background cleanup loops for both metadata formats.
// Reaping expired entries also decrements their blob reference counts, making
// those blobs eligible for S3-FIFO eviction and garbage collection.
type metadataReapers struct {
	expiry   *metadb.ExpiryReaper
	envelope *metadb.EnvelopeReaper

	cancel context.CancelFunc
	done   sync.WaitGroup
}

func newMetadataReapers(db metadb.MetaDB, interval time.Duration, logger *slog.Logger) *metadataReapers {
	return &metadataReapers{
		expiry: metadb.NewExpiryReaper(db,
			metadb.WithReaperInterval(interval),
			metadb.WithReaperLogger(logger.With("component", "expiry-reaper")),
		),
		envelope: metadb.NewEnvelopeReaper(db,
			metadb.WithEnvelopeReaperInterval(interval),
			metadb.WithEnvelopeReaperLogger(logger.With("component", "envelope-reaper")),
		),
	}
}

func (r *metadataReapers) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel

	r.done.Add(2)
	go func() {
		defer r.done.Done()
		r.expiry.Run(ctx)
	}()
	go func() {
		defer r.done.Done()
		r.envelope.Run(ctx)
	}()
}

func (r *metadataReapers) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.done.Wait()
}
