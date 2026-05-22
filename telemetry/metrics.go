package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const (
	meterName = "github.com/buildkite/content-cache"
)

// MetricsConfig configures the metrics system. OTLP transport is driven by
// the standard OTEL_* environment variables (see README); ServiceName and
// ServiceVersion are used only as fallbacks when the corresponding env vars
// are not set.
type MetricsConfig struct {
	ServiceName      string
	ServiceVersion   string
	EnablePrometheus bool
}

// Metrics holds the OpenTelemetry metric instruments.
type Metrics struct {
	requestsTotal           metric.Int64Counter
	responseBytesTotal      metric.Int64Counter
	requestDuration         metric.Float64Histogram
	requestsByEndpointTotal metric.Int64Counter

	blobWriteSize           metric.Float64Histogram
	upstreamFetchDuration   metric.Float64Histogram
	upstreamFetchTotal      metric.Int64Counter
	upstreamFetchBytesTotal metric.Int64Counter
	blobTouchesTotal        metric.Int64Counter
	blobTouchMissesTotal    metric.Int64Counter
	backendRequestDuration  metric.Float64Histogram
	backendRequestsTotal    metric.Int64Counter
	backendBytesTotal       metric.Int64Counter

	// Auth metrics
	authRequestsTotal metric.Int64Counter

	// Reaper metrics
	reaperDeletedTotal metric.Int64Counter
	reaperDuration     metric.Float64Histogram

	// S3-FIFO eviction metrics
	s3fifoAdmissionsTotal          metric.Int64Counter
	s3fifoAdmissionBytesTotal      metric.Int64Counter
	s3fifoGhostHitsTotal           metric.Int64Counter
	s3fifoPromotionsTotal          metric.Int64Counter
	s3fifoOneHitEvictionsTotal     metric.Int64Counter
	s3fifoOneHitEvictionBytesTotal metric.Int64Counter
	s3fifoSecondChanceTotal        metric.Int64Counter
	s3fifoEvictionsTotal           metric.Int64Counter
	s3fifoEvictionBytesTotal       metric.Int64Counter
	s3fifoPinnedSkipsTotal         metric.Int64Counter
	s3fifoEvictionRunDuration      metric.Float64Histogram
	s3fifoEvictionRunsTotal        metric.Int64Counter
	s3fifoQueueBytes               metric.Int64Gauge
	s3fifoQueueEntries             metric.Int64Gauge
	s3fifoGhostEntries             metric.Int64Gauge
	s3fifoTargetBytes              metric.Int64Gauge
	s3fifoCacheMaxSizeBytes        metric.Int64Gauge
	s3fifoOverlimitBytes           metric.Int64Gauge

	meterProvider *sdkmetric.MeterProvider
	promHandler   http.Handler
}

var (
	globalMetrics *Metrics
	initOnce      sync.Once
	initErr       error
)

// InitMetrics initializes the OpenTelemetry metrics system.
// Returns a shutdown function that should be called on application exit.
// Uses sync.Once to ensure single initialisation.
func InitMetrics(ctx context.Context, cfg MetricsConfig) (shutdown func(context.Context) error, err error) {
	initOnce.Do(func() {
		initErr = doInitMetrics(ctx, cfg)
	})

	if initErr != nil {
		return nil, initErr
	}

	return shutdownMetrics, nil
}

func doInitMetrics(ctx context.Context, cfg MetricsConfig) error {
	res, err := buildResource(cfg)
	if err != nil {
		return err
	}

	var readers []sdkmetric.Reader
	var promHandler http.Handler

	if otlpExportEnabled() {
		otlpReader, err := newOTLPReader(ctx)
		if err != nil {
			return err
		}
		readers = append(readers, otlpReader)
	}

	if cfg.EnablePrometheus {
		promExp, err := promexporter.New()
		if err != nil {
			return err
		}
		readers = append(readers, promExp)
		promHandler = promhttp.Handler()
	}

	// If no exporters configured, use a no-op periodic reader to still collect metrics
	if len(readers) == 0 {
		readers = append(readers, sdkmetric.NewPeriodicReader(noopExporter{}))
	}

	// Build meter provider options
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)

	// Create meter and instruments
	meter := mp.Meter(meterName)

	requestsTotal, err := meter.Int64Counter(
		"content_cache_http_requests_total",
		metric.WithDescription("Total number of HTTP requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	responseBytesTotal, err := meter.Int64Counter(
		"content_cache_http_response_bytes_total",
		metric.WithDescription("Total bytes sent in HTTP responses"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	requestDuration, err := meter.Float64Histogram(
		"content_cache_http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return err
	}

	requestsByEndpointTotal, err := meter.Int64Counter(
		"content_cache_http_requests_by_endpoint_total",
		metric.WithDescription("Total number of HTTP requests by endpoint (detail metric)"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	authRequestsTotal, err := meter.Int64Counter(
		"content_cache_auth_requests_total",
		metric.WithDescription("Total OIDC-authenticated requests by outcome and protocol"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	blobWriteSize, err := meter.Float64Histogram(
		"content_cache_blob_write_size_bytes",
		metric.WithDescription("Size of blobs written to storage"),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(128, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 4194304, 8388608, 16777216, 33554432, 67108864, 134217728, 268435456, 536870912, 1073741824),
	)
	if err != nil {
		return err
	}

	upstreamFetchDuration, err := meter.Float64Histogram(
		"content_cache_upstream_fetch_duration_seconds",
		metric.WithDescription("Duration of upstream fetch requests"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 60),
	)
	if err != nil {
		return err
	}

	upstreamFetchTotal, err := meter.Int64Counter(
		"content_cache_upstream_fetch_total",
		metric.WithDescription("Total number of upstream fetch requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	upstreamFetchBytesTotal, err := meter.Int64Counter(
		"content_cache_upstream_fetch_bytes_total",
		metric.WithDescription("Total bytes fetched from upstream"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	blobTouchesTotal, err := meter.Int64Counter(
		"content_cache_blob_touches_total",
		metric.WithDescription("Total blob access count increments"),
		metric.WithUnit("{touch}"),
	)
	if err != nil {
		return err
	}

	blobTouchMissesTotal, err := meter.Int64Counter(
		"content_cache_blob_touch_misses_total",
		metric.WithDescription("Total blob touch attempts where blob exists on disk but not in metadata"),
		metric.WithUnit("{miss}"),
	)
	if err != nil {
		return err
	}

	backendRequestDuration, err := meter.Float64Histogram(
		"content_cache_backend_request_duration_seconds",
		metric.WithDescription("Duration of backend storage operations"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)
	if err != nil {
		return err
	}

	backendRequestsTotal, err := meter.Int64Counter(
		"content_cache_backend_requests_total",
		metric.WithDescription("Total number of backend storage operations"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	backendBytesTotal, err := meter.Int64Counter(
		"content_cache_backend_bytes_total",
		metric.WithDescription("Total bytes transferred in backend operations"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	reaperDeletedTotal, err := meter.Int64Counter(
		"content_cache_reaper_deleted_total",
		metric.WithDescription("Total entries deleted by reapers"),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return err
	}

	reaperDuration, err := meter.Float64Histogram(
		"content_cache_reaper_duration_seconds",
		metric.WithDescription("Duration of reaper cycles"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30),
	)
	if err != nil {
		return err
	}

	s3fifoAdmissionsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_admissions_total",
		metric.WithDescription("Total blobs admitted to S3-FIFO queues"),
		metric.WithUnit("{blob}"),
	)
	if err != nil {
		return err
	}

	s3fifoAdmissionBytesTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_admission_bytes_total",
		metric.WithDescription("Total bytes admitted to S3-FIFO queues"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoGhostHitsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_ghost_hits_total",
		metric.WithDescription("Total ghost queue hits (scan resistance signal)"),
		metric.WithUnit("{hit}"),
	)
	if err != nil {
		return err
	}

	s3fifoPromotionsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_promotions_total",
		metric.WithDescription("Total small→main promotions (accessCount > 0 at eviction)"),
		metric.WithUnit("{blob}"),
	)
	if err != nil {
		return err
	}

	s3fifoOneHitEvictionsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_one_hit_evictions_total",
		metric.WithDescription("Total one-hit wonders filtered (accessCount == 0 evicted from small)"),
		metric.WithUnit("{blob}"),
	)
	if err != nil {
		return err
	}

	s3fifoOneHitEvictionBytesTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_one_hit_eviction_bytes_total",
		metric.WithDescription("Total bytes freed by filtering one-hit wonders"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoSecondChanceTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_second_chance_total",
		metric.WithDescription("Total main queue reinsertions (accessCount > 0, decremented)"),
		metric.WithUnit("{blob}"),
	)
	if err != nil {
		return err
	}

	s3fifoEvictionsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_evictions_total",
		metric.WithDescription("Total evictions from S3-FIFO queues"),
		metric.WithUnit("{blob}"),
	)
	if err != nil {
		return err
	}

	s3fifoEvictionBytesTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_eviction_bytes_total",
		metric.WithDescription("Total bytes freed by S3-FIFO eviction"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoPinnedSkipsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_pinned_skips_total",
		metric.WithDescription("Total eviction skips due to RefCount > 0"),
		metric.WithUnit("{skip}"),
	)
	if err != nil {
		return err
	}

	s3fifoEvictionRunDuration, err := meter.Float64Histogram(
		"content_cache_s3fifo_eviction_run_duration_seconds",
		metric.WithDescription("Duration of S3-FIFO eviction runs"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return err
	}

	s3fifoEvictionRunsTotal, err := meter.Int64Counter(
		"content_cache_s3fifo_eviction_runs_total",
		metric.WithDescription("Total S3-FIFO eviction runs"),
		metric.WithUnit("{run}"),
	)
	if err != nil {
		return err
	}

	s3fifoQueueBytes, err := meter.Int64Gauge(
		"content_cache_s3fifo_queue_bytes",
		metric.WithDescription("Current bytes in each S3-FIFO queue"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoQueueEntries, err := meter.Int64Gauge(
		"content_cache_s3fifo_queue_entries",
		metric.WithDescription("Current entries in each S3-FIFO queue"),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return err
	}

	s3fifoGhostEntries, err := meter.Int64Gauge(
		"content_cache_s3fifo_ghost_entries",
		metric.WithDescription("Current entries in the S3-FIFO ghost set"),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return err
	}

	s3fifoTargetBytes, err := meter.Int64Gauge(
		"content_cache_s3fifo_target_bytes",
		metric.WithDescription("S3-FIFO small queue target size (MaxSize × SmallQueuePercent)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoCacheMaxSizeBytes, err := meter.Int64Gauge(
		"content_cache_s3fifo_cache_max_size_bytes",
		metric.WithDescription("Configured maximum cache size"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	s3fifoOverlimitBytes, err := meter.Int64Gauge(
		"content_cache_s3fifo_overlimit_bytes",
		metric.WithDescription("Bytes over the cache limit (pressure indicator)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	globalMetrics = &Metrics{
		authRequestsTotal:              authRequestsTotal,
		reaperDeletedTotal:             reaperDeletedTotal,
		reaperDuration:                 reaperDuration,
		requestsTotal:                  requestsTotal,
		responseBytesTotal:             responseBytesTotal,
		requestDuration:                requestDuration,
		requestsByEndpointTotal:        requestsByEndpointTotal,
		blobWriteSize:                  blobWriteSize,
		upstreamFetchDuration:          upstreamFetchDuration,
		upstreamFetchTotal:             upstreamFetchTotal,
		upstreamFetchBytesTotal:        upstreamFetchBytesTotal,
		blobTouchesTotal:               blobTouchesTotal,
		blobTouchMissesTotal:           blobTouchMissesTotal,
		backendRequestDuration:         backendRequestDuration,
		backendRequestsTotal:           backendRequestsTotal,
		backendBytesTotal:              backendBytesTotal,
		s3fifoAdmissionsTotal:          s3fifoAdmissionsTotal,
		s3fifoAdmissionBytesTotal:      s3fifoAdmissionBytesTotal,
		s3fifoGhostHitsTotal:           s3fifoGhostHitsTotal,
		s3fifoPromotionsTotal:          s3fifoPromotionsTotal,
		s3fifoOneHitEvictionsTotal:     s3fifoOneHitEvictionsTotal,
		s3fifoOneHitEvictionBytesTotal: s3fifoOneHitEvictionBytesTotal,
		s3fifoSecondChanceTotal:        s3fifoSecondChanceTotal,
		s3fifoEvictionsTotal:           s3fifoEvictionsTotal,
		s3fifoEvictionBytesTotal:       s3fifoEvictionBytesTotal,
		s3fifoPinnedSkipsTotal:         s3fifoPinnedSkipsTotal,
		s3fifoEvictionRunDuration:      s3fifoEvictionRunDuration,
		s3fifoEvictionRunsTotal:        s3fifoEvictionRunsTotal,
		s3fifoQueueBytes:               s3fifoQueueBytes,
		s3fifoQueueEntries:             s3fifoQueueEntries,
		s3fifoGhostEntries:             s3fifoGhostEntries,
		s3fifoTargetBytes:              s3fifoTargetBytes,
		s3fifoCacheMaxSizeBytes:        s3fifoCacheMaxSizeBytes,
		s3fifoOverlimitBytes:           s3fifoOverlimitBytes,
		meterProvider:                  mp,
		promHandler:                    promHandler,
	}

	return nil
}

// shutdownMetrics shuts down the metrics provider and clears the global state.
func shutdownMetrics(ctx context.Context) error {
	if globalMetrics == nil {
		return nil
	}
	err := globalMetrics.meterProvider.Shutdown(ctx)
	globalMetrics = nil
	return err
}

func buildResource(cfg MetricsConfig) (*resource.Resource, error) {
	return buildResourceFrom(resource.Default(), cfg)
}

// buildResourceFrom is split from buildResource so tests can supply a base
// resource — resource.Default() is a process-wide cached singleton.
func buildResourceFrom(base *resource.Resource, cfg MetricsConfig) (*resource.Resource, error) {
	var fallbackAttrs []attribute.KeyValue
	if cfg.ServiceName != "" && !hasResourceAttr(base, "service.name") {
		fallbackAttrs = append(fallbackAttrs, semconv.ServiceName(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" && !hasResourceAttr(base, "service.version") {
		fallbackAttrs = append(fallbackAttrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if len(fallbackAttrs) == 0 {
		return base, nil
	}
	return resource.Merge(base, resource.NewWithAttributes(semconv.SchemaURL, fallbackAttrs...))
}

// hasResourceAttr reports whether the resource has a real value for key.
// The SDK seeds service.name with "unknown_service:<binary>" when neither
// OTEL_SERVICE_NAME nor OTEL_RESOURCE_ATTRIBUTES sets it; that placeholder
// is treated as absent so the configured fallback can take over.
func hasResourceAttr(r *resource.Resource, key string) bool {
	for _, a := range r.Attributes() {
		if string(a.Key) != key {
			continue
		}
		v := a.Value.AsString()
		if key == "service.name" && strings.HasPrefix(v, "unknown_service:") {
			return false
		}
		return v != ""
	}
	return false
}

func otlpExportEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

func otlpProtocol() string {
	if p := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"); p != "" {
		return p
	}
	if p := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"); p != "" {
		return p
	}
	return "http/protobuf"
}

// newOTLPReader constructs a PeriodicReader backed by an OTLP exporter
// whose transport (gRPC vs HTTP/protobuf) is chosen by otlpProtocol().
//
// The underlying exporter is intentionally created with zero options. When
// called this way, otlpmetricgrpc.New / otlpmetrichttp.New auto-configure
// themselves from the standard OTel env vars, including:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
//   - OTEL_EXPORTER_OTLP_INSECURE, OTEL_EXPORTER_OTLP_METRICS_INSECURE
//   - OTEL_EXPORTER_OTLP_HEADERS, OTEL_EXPORTER_OTLP_METRICS_HEADERS
//   - OTEL_EXPORTER_OTLP_COMPRESSION, OTEL_EXPORTER_OTLP_METRICS_COMPRESSION
//   - OTEL_EXPORTER_OTLP_TIMEOUT, OTEL_EXPORTER_OTLP_METRICS_TIMEOUT
//   - OTEL_EXPORTER_OTLP_CERTIFICATE, OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE,
//     OTEL_EXPORTER_OTLP_CLIENT_KEY (and their _METRICS_ variants for mTLS)
//
// Passing any explicit option to New() (e.g. WithEndpoint) would NOT merge
// with env-derived defaults — it would replace them — so adding even one
// option here silently disables env-based configuration of every other
// setting. That was the bug in the previous implementation: it set
// WithEndpoint + WithInsecure and made OTEL_EXPORTER_OTLP_HEADERS, _TIMEOUT,
// _COMPRESSION, mTLS, etc. unreachable. Keep the call optionless.
//
// Protocol selection is handled here rather than by the SDK because Go's
// OTel exporters live in two separate packages (otlpmetricgrpc,
// otlpmetrichttp) and the SDK does not auto-dispatch between them based on
// OTEL_EXPORTER_OTLP_PROTOCOL. The caller must pick.
//
// PeriodicReader is likewise constructed with zero options so it reads
// OTEL_METRIC_EXPORT_INTERVAL.
func newOTLPReader(ctx context.Context) (sdkmetric.Reader, error) {
	var exporter sdkmetric.Exporter
	var err error

	switch p := otlpProtocol(); p {
	case "grpc":
		exporter, err = otlpmetricgrpc.New(ctx)
	case "http/protobuf":
		exporter, err = otlpmetrichttp.New(ctx)
	default:
		return nil, fmt.Errorf("unsupported OTEL_EXPORTER_OTLP_PROTOCOL %q (supported: grpc, http/protobuf)", p)
	}
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewPeriodicReader(exporter), nil
}

// RecordHTTP records HTTP request metrics.
// Call this from the logging middleware after the request completes.
// Protocol and cache result are read from request tags set by middleware and handlers.
func RecordHTTP(ctx context.Context, r *http.Request, status int, bytesSent int64, duration time.Duration) {
	if globalMetrics == nil {
		return
	}

	tags := GetTags(r)

	protocol := "unknown"
	cacheResult := string(CacheBypass)
	endpoint := ""
	if tags != nil {
		if tags.Protocol != "" {
			protocol = tags.Protocol
		}
		if tags.CacheResult != "" {
			cacheResult = string(tags.CacheResult)
		}
		endpoint = tags.Endpoint
	}

	statusClass := StatusClass(status)

	// Shared metrics: low cardinality {protocol, status_class, cache_result}
	sharedAttrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
		attribute.String("status_class", statusClass),
		attribute.String("cache_result", cacheResult),
	}
	globalMetrics.requestsTotal.Add(ctx, 1, metric.WithAttributes(sharedAttrs...))
	globalMetrics.responseBytesTotal.Add(ctx, bytesSent, metric.WithAttributes(sharedAttrs...))
	globalMetrics.requestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(sharedAttrs...))

	// Detail metric: higher cardinality, only when endpoint is set
	if endpoint != "" {
		detailAttrs := []attribute.KeyValue{
			attribute.String("protocol", protocol),
			attribute.String("endpoint", endpoint),
			attribute.String("status_class", statusClass),
			attribute.String("cache_result", cacheResult),
		}
		globalMetrics.requestsByEndpointTotal.Add(ctx, 1, metric.WithAttributes(detailAttrs...))
	}

	// Auth metric: only for OIDC-protected routes where auth outcome was set
	if tags != nil && tags.AuthOutcome != "" {
		globalMetrics.authRequestsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("protocol", protocol),
			attribute.String("outcome", string(tags.AuthOutcome)),
		))
	}
}

// RecordBackendOp records backend operation metrics.
func RecordBackendOp(ctx context.Context, backend, op, outcome string, duration time.Duration, bytes int64) {
	if globalMetrics == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("backend", backend),
		attribute.String("op", op),
		attribute.String("outcome", outcome),
	}
	globalMetrics.backendRequestsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	globalMetrics.backendRequestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	if bytes > 0 {
		globalMetrics.backendBytesTotal.Add(ctx, bytes, metric.WithAttributes(attrs...))
	}
}

// RecordBlobWrite records a blob write with its size.
func RecordBlobWrite(ctx context.Context, protocol string, size int64, isNew bool) {
	if globalMetrics == nil {
		return
	}

	result := "exists"
	if isNew {
		result = "new"
	}

	attrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
		attribute.String("result", result),
	}
	globalMetrics.blobWriteSize.Record(ctx, float64(size), metric.WithAttributes(attrs...))
}

// RecordUpstreamFetch records an upstream fetch request.
func RecordUpstreamFetch(ctx context.Context, protocol string, duration time.Duration, bytesRead int64, outcome string) {
	if globalMetrics == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
		attribute.String("outcome", outcome),
	}
	globalMetrics.upstreamFetchDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	globalMetrics.upstreamFetchTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	if bytesRead > 0 {
		globalMetrics.upstreamFetchBytesTotal.Add(ctx, bytesRead, metric.WithAttributes(attrs...))
	}
}

// RecordBlobTouch records a blob access count increment.
func RecordBlobTouch(ctx context.Context, protocol string, newAccessCount int) {
	if globalMetrics == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
		attribute.String("new_access_count", strconv.Itoa(newAccessCount)),
	}
	globalMetrics.blobTouchesTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordBlobTouchMiss records a blob touch attempt where the blob exists on
// disk but has no corresponding metadata row in the database.
func RecordBlobTouchMiss(ctx context.Context, protocol string) {
	if globalMetrics == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
	}
	globalMetrics.blobTouchMissesTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// PrometheusHandler returns the Prometheus metrics HTTP handler.
// Returns a handler that returns 404 if Prometheus export is not enabled,
// allowing safe registration regardless of initialization order.
func PrometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if globalMetrics == nil || globalMetrics.promHandler == nil {
			http.NotFound(w, r)
			return
		}
		globalMetrics.promHandler.ServeHTTP(w, r)
	})
}

// RecordS3FIFOAdmission records a blob admission to an S3-FIFO queue.
// queue is "small" or "main", reason is "new" or "ghost_hit".
func RecordS3FIFOAdmission(ctx context.Context, queue, reason string, bytes int64) {
	if globalMetrics == nil {
		return
	}
	protocol := ProtocolFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("protocol", protocol),
		attribute.String("queue", queue),
		attribute.String("reason", reason),
	}
	globalMetrics.s3fifoAdmissionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	globalMetrics.s3fifoAdmissionBytesTotal.Add(ctx, bytes, metric.WithAttributes(attrs...))
}

// RecordS3FIFOGhostHit records a ghost queue hit.
func RecordS3FIFOGhostHit(ctx context.Context) {
	if globalMetrics == nil {
		return
	}
	protocol := ProtocolFromContext(ctx)
	attrs := []attribute.KeyValue{attribute.String("protocol", protocol)}
	globalMetrics.s3fifoGhostHitsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordS3FIFOPromotion records a small→main promotion.
func RecordS3FIFOPromotion(ctx context.Context) {
	if globalMetrics == nil {
		return
	}
	protocol := ProtocolFromContext(ctx)
	attrs := []attribute.KeyValue{attribute.String("protocol", protocol)}
	globalMetrics.s3fifoPromotionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordS3FIFOOneHitEviction records a one-hit-wonder eviction from the small queue.
func RecordS3FIFOOneHitEviction(ctx context.Context, bytes int64) {
	if globalMetrics == nil {
		return
	}
	protocol := ProtocolFromContext(ctx)
	attrs := []attribute.KeyValue{attribute.String("protocol", protocol)}
	globalMetrics.s3fifoOneHitEvictionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	globalMetrics.s3fifoOneHitEvictionBytesTotal.Add(ctx, bytes, metric.WithAttributes(attrs...))
}

// RecordS3FIFOSecondChance records a main queue second-chance reinsertion.
func RecordS3FIFOSecondChance(ctx context.Context) {
	if globalMetrics == nil {
		return
	}
	protocol := ProtocolFromContext(ctx)
	attrs := []attribute.KeyValue{attribute.String("protocol", protocol)}
	globalMetrics.s3fifoSecondChanceTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordS3FIFOEviction records a final eviction from a queue.
// queue is "small" or "main".
func RecordS3FIFOEviction(ctx context.Context, queue string, bytes int64) {
	if globalMetrics == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("queue", queue)}
	globalMetrics.s3fifoEvictionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	globalMetrics.s3fifoEvictionBytesTotal.Add(ctx, bytes, metric.WithAttributes(attrs...))
}

// RecordS3FIFOPinnedSkip records a skipped eviction due to RefCount > 0.
// queue is "small" or "main".
func RecordS3FIFOPinnedSkip(ctx context.Context, queue string) {
	if globalMetrics == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("queue", queue)}
	globalMetrics.s3fifoPinnedSkipsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordS3FIFOEvictionRun records the duration of one MaybeEvict call.
func RecordS3FIFOEvictionRun(ctx context.Context, duration time.Duration) {
	if globalMetrics == nil {
		return
	}
	globalMetrics.s3fifoEvictionRunDuration.Record(ctx, duration.Seconds())
	globalMetrics.s3fifoEvictionRunsTotal.Add(ctx, 1)
}

// UpdateS3FIFOQueueState updates all S3-FIFO queue-state gauges.
// Called synchronously at the end of each MaybeEvict run.
func UpdateS3FIFOQueueState(ctx context.Context, smallBytes, mainBytes int64, smallEntries, mainEntries, ghostEntries int, maxBytes, targetBytes int64) {
	if globalMetrics == nil {
		return
	}
	globalMetrics.s3fifoQueueBytes.Record(ctx, smallBytes, metric.WithAttributes(attribute.String("queue", "small")))
	globalMetrics.s3fifoQueueBytes.Record(ctx, mainBytes, metric.WithAttributes(attribute.String("queue", "main")))
	globalMetrics.s3fifoQueueEntries.Record(ctx, int64(smallEntries), metric.WithAttributes(attribute.String("queue", "small")))
	globalMetrics.s3fifoQueueEntries.Record(ctx, int64(mainEntries), metric.WithAttributes(attribute.String("queue", "main")))
	globalMetrics.s3fifoGhostEntries.Record(ctx, int64(ghostEntries))
	globalMetrics.s3fifoTargetBytes.Record(ctx, targetBytes)
	globalMetrics.s3fifoCacheMaxSizeBytes.Record(ctx, maxBytes)
	total := smallBytes + mainBytes
	overlimit := total - maxBytes
	if overlimit < 0 {
		overlimit = 0
	}
	globalMetrics.s3fifoOverlimitBytes.Record(ctx, overlimit)
}

// RecordReaperCycle records one reaper cycle's deleted count and duration.
// reaper is "envelope" or "expiry". Called unconditionally per cycle.
func RecordReaperCycle(ctx context.Context, reaper string, deleted int, duration time.Duration) {
	if globalMetrics == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("reaper", reaper))
	globalMetrics.reaperDeletedTotal.Add(ctx, int64(deleted), attrs)
	globalMetrics.reaperDuration.Record(ctx, duration.Seconds(), attrs)
}

// StatusClass returns the HTTP status class (2xx, 3xx, 4xx, 5xx).
func StatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "unknown"
	}
}

// noopExporter is a no-op metrics exporter for when no exporters are configured.
type noopExporter struct{}

func (noopExporter) Temporality(_ sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

func (noopExporter) Aggregation(_ sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return nil
}

func (noopExporter) Export(_ context.Context, _ *metricdata.ResourceMetrics) error {
	return nil
}

func (noopExporter) ForceFlush(_ context.Context) error {
	return nil
}

func (noopExporter) Shutdown(_ context.Context) error {
	return nil
}
