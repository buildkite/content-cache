package npm

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/buildkite/content-cache/telemetry"
)

// metadataFetch holds immutable full metadata shared by concurrent requests.
// All result fields are published by closing done before any caller reads them.
type metadataFetch struct {
	done   chan struct{}
	data   []byte
	err    error
	cached bool
}

// startMetadataFetch joins an existing fetch or starts one handler-scoped worker.
// The mutex protects registration only; different packages fetch concurrently.
func (h *Handler) startMetadataFetch(name string) (*metadataFetch, bool, error) {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	if err := h.shutdownCtx.Err(); err != nil {
		return nil, false, err
	}
	if flight, ok := h.metadataFlights[name]; ok {
		return flight, true, nil
	}
	flight := &metadataFetch{done: make(chan struct{})}
	h.metadataFlights[name] = flight
	h.wg.Go(func() {
		ctx, cancel := context.WithTimeout(h.shutdownCtx, cacheTimeout)
		defer cancel()
		ctx = telemetry.WithProtocolContext(ctx, "npm")
		flight.data, flight.cached, flight.err = h.fetchAndCacheMetadata(ctx, name)
		h.metadataMu.Lock()
		defer h.metadataMu.Unlock()
		delete(h.metadataFlights, name)
		close(flight.done)
	})
	return flight, false, nil
}

func (h *Handler) fetchAndCacheMetadata(ctx context.Context, name string) ([]byte, bool, error) {
	logger := h.logger.With("package", name, "endpoint", "metadata")
	// A prior worker may have populated the cache between the initial lookup
	// and registration. Recheck to avoid another upstream request in that gap.
	if data, err := h.index.GetPackageMetadata(ctx, name); err == nil {
		return data, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		logger.Error("cache read failed", "error", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	logger.Debug("cache miss, fetching shared metadata from upstream")
	meta, err := h.router.Match(name).FetchPackageMetadataDecoded(ctx, name)
	if err != nil {
		return nil, false, err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, false, err
	}
	// Keep the flight registered through the write so later requests cannot
	// miss both the in-flight result and the cache. A write failure must not
	// prevent delivery of a valid upstream response to current waiters.
	if err := h.index.PutPackageMetadata(ctx, name, data); err != nil {
		logger.Error("failed to cache metadata", "error", err)
	} else {
		logger.Debug("cached metadata")
	}
	return data, false, nil
}
