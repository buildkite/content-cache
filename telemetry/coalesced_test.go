package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecordHTTPCoalescedMetadata(t *testing.T) {
	reader := setupTestMetrics(t)
	r := InjectTags(httptest.NewRequest(http.MethodGet, "/npm/pkg", nil))
	SetProtocol(r, "npm")
	SetEndpoint(r, "metadata")
	SetCacheResult(r, CacheCoalesced)
	RecordHTTP(context.Background(), r, http.StatusOK, 100, time.Millisecond)
	rm := collectMetrics(t, reader)
	for _, name := range []string{"content_cache_http_requests_total", "content_cache_http_requests_by_endpoint_total"} {
		points := findCounter(rm, name)
		require.Len(t, points, 1)
		require.True(t, hasAttr(points[0].Attributes, "cache_result", "coalesced"))
	}
}
