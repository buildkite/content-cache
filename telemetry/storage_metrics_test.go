package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStorageOperationalMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(meterName)
	m := &Metrics{}
	var err error
	m.metaDBBatchDuration, err = meter.Float64Histogram("content_cache_metadb_batch_duration_seconds")
	require.NoError(t, err)
	m.metaDBLastWriteTransaction, err = meter.Int64Gauge("content_cache_metadb_last_write_transaction_id")
	require.NoError(t, err)
	m.metaDBWriteCalls, err = meter.Int64Gauge("content_cache_metadb_write_calls")
	require.NoError(t, err)
	m.metaDBWriteTime, err = meter.Float64Gauge("content_cache_metadb_write_time_seconds")
	require.NoError(t, err)
	m.s3fifoEvictionErrorsTotal, err = meter.Int64Counter("content_cache_s3fifo_eviction_errors_total")
	require.NoError(t, err)
	m.s3fifoEvictionBlockedTotal, err = meter.Int64Counter("content_cache_s3fifo_eviction_blocked_total")
	require.NoError(t, err)
	m.s3fifoOrphanedQueueEntriesTotal, err = meter.Int64Counter("content_cache_s3fifo_orphaned_queue_entries_total")
	require.NoError(t, err)
	globalMetrics = m
	t.Cleanup(func() { globalMetrics = nil; _ = provider.Shutdown(context.Background()) })
	ctx := t.Context()
	RecordMetaDBBatch(ctx, "put_envelope", "success", 5*time.Millisecond, time.Millisecond)
	UpdateMetaDBStats(ctx, 42, 12, time.Second)
	RecordS3FIFOEvictionError(ctx, "small", "backend_delete")
	RecordS3FIFOEvictionBlocked(ctx, "all_candidates_pinned")
	RecordS3FIFOOrphanedQueueEntry(ctx, "main")
	rm := collectMetrics(t, reader)
	phases := findHistogram(rm, "content_cache_metadb_batch_duration_seconds")
	require.Len(t, phases, 2)
	for _, point := range phases {
		require.True(t, hasAttr(point.Attributes, "operation", "put_envelope"))
		require.True(t, hasAttr(point.Attributes, "outcome", "success"))
		if hasAttr(point.Attributes, "phase", "total") {
			require.InDelta(t, 0.005, point.Sum, 1e-9)
		} else {
			require.True(t, hasAttr(point.Attributes, "phase", "callback"))
			require.InDelta(t, 0.001, point.Sum, 1e-9)
		}
	}
	for _, name := range []string{"content_cache_s3fifo_eviction_errors_total", "content_cache_s3fifo_eviction_blocked_total", "content_cache_s3fifo_orphaned_queue_entries_total"} {
		points := findCounter(rm, name)
		require.Len(t, points, 1)
		require.EqualValues(t, 1, points[0].Value)
	}
	gauges := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch metric.Name {
			case "content_cache_metadb_last_write_transaction_id":
				gauge, ok := metric.Data.(metricdata.Gauge[int64])
				require.True(t, ok)
				require.Len(t, gauge.DataPoints, 1)
				require.EqualValues(t, 42, gauge.DataPoints[0].Value)
				gauges[metric.Name] = true
			case "content_cache_metadb_write_calls":
				gauge, ok := metric.Data.(metricdata.Gauge[int64])
				require.True(t, ok)
				require.Len(t, gauge.DataPoints, 1)
				require.EqualValues(t, 12, gauge.DataPoints[0].Value)
				gauges[metric.Name] = true
			case "content_cache_metadb_write_time_seconds":
				gauge, ok := metric.Data.(metricdata.Gauge[float64])
				require.True(t, ok)
				require.Len(t, gauge.DataPoints, 1)
				require.Equal(t, 1.0, gauge.DataPoints[0].Value)
				gauges[metric.Name] = true
			}
		}
	}
	require.Len(t, gauges, 3)
}
