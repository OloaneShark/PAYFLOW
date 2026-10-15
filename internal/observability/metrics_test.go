package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrometheusMetricsHaveBoundedNames(t *testing.T) {
	r := New()
	r.Inc("payflow_processing_attempts_total")
	r.Inc("payflow_approvals_total")
	r.Observe("payflow_provider_response_seconds", 150*time.Millisecond)
	r.Set("payflow_reconciliation_backlog", 2)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, expected := range []string{"payflow_processing_attempts_total 1", "payflow_approvals_total 1", "payflow_provider_response_seconds_count 1", "payflow_reconciliation_backlog 2"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in %s", expected, body)
		}
	}
	if strings.Contains(body, "{") || strings.Contains(body, "merchant") || strings.Contains(body, "payment_id") {
		t.Fatalf("high-cardinality labels: %s", body)
	}
}
