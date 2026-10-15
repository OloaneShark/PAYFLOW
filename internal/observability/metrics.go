package observability

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Registry has a fixed, label-free metric vocabulary. IDs, keys, URLs, and
// payloads can never become Prometheus labels through this API.
type Registry struct {
	mu            sync.Mutex
	values        map[string]float64
	otelCounters  map[string]metric.Int64Counter
	otelDurations map[string]metric.Float64Histogram
}

var counters = map[string]bool{
	"payflow_processing_attempts_total":         true,
	"payflow_approvals_total":                   true,
	"payflow_declines_total":                    true,
	"payflow_reconciliation_attempts_total":     true,
	"payflow_reconciliation_failures_total":     true,
	"payflow_abandoned_claims_total":            true,
	"payflow_workflow_dispatch_failures_total":  true,
	"payflow_database_errors_total":             true,
	"payflow_ledger_posting_failures_total":     true,
	"payflow_ledger_invariant_violations_total": true,
}
var durations = map[string]bool{
	"payflow_provider_response_seconds": true,
	"payflow_api_response_seconds":      true,
}
var gauges = map[string]bool{
	"payflow_reconciliation_backlog":      true,
	"payflow_stuck_processing_claims":     true,
	"payflow_ledger_integrity_violations": true,
}

func New() *Registry {
	r := &Registry{values: make(map[string]float64), otelCounters: make(map[string]metric.Int64Counter), otelDurations: make(map[string]metric.Float64Histogram)}
	meter := otel.Meter("payflow")
	for name := range counters {
		counter, err := meter.Int64Counter(name)
		if err == nil {
			r.otelCounters[name] = counter
		}
	}
	for name := range durations {
		histogram, err := meter.Float64Histogram(name)
		if err == nil {
			r.otelDurations[name] = histogram
		}
	}
	return r
}

func (r *Registry) Inc(name string) {
	if !counters[name] {
		panic("unknown metric: " + name)
	}
	r.mu.Lock()
	r.values[name]++
	r.mu.Unlock()
	if counter := r.otelCounters[name]; counter != nil {
		counter.Add(context.Background(), 1)
	}
}

func (r *Registry) Observe(name string, d time.Duration) {
	if !durations[name] {
		panic("unknown duration metric: " + name)
	}
	r.mu.Lock()
	r.values[name+"_count"]++
	r.values[name+"_sum"] += d.Seconds()
	r.mu.Unlock()
	if histogram := r.otelDurations[name]; histogram != nil {
		histogram.Record(context.Background(), d.Seconds())
	}
}

func (r *Registry) Set(name string, value float64) {
	if !gauges[name] {
		panic("unknown gauge: " + name)
	}
	r.mu.Lock()
	r.values[name] = value
	r.mu.Unlock()
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(counters)+len(durations)*2+len(gauges))
	for k := range counters {
		keys = append(keys, k)
	}
	for k := range durations {
		keys = append(keys, k+"_count", k+"_sum")
	}
	for k := range gauges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s %s\n", k, strconv.FormatFloat(r.values[k], 'f', -1, 64))
	}
}
