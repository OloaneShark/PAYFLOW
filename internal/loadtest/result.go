package loadtest

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"payflow/internal/verification"
)

const label = "LOCAL SIMULATED PAYMENT BENCHMARK"

type Latency struct {
	AverageMS float64 `json:"average_ms"`
	MedianMS  float64 `json:"median_ms"`
	P95MS     float64 `json:"p95_ms"`
	P99MS     float64 `json:"p99_ms"`
	MaximumMS float64 `json:"maximum_ms"`
}

type AsyncTiming struct {
	Count     int     `json:"count"`
	AverageMS float64 `json:"average_ms"`
}

type Result struct {
	Label              string              `json:"label"`
	TestName           string              `json:"test_name"`
	DateUTC            time.Time           `json:"date_utc"`
	DurationSeconds    float64             `json:"duration_seconds"`
	WarmupSeconds      float64             `json:"warmup_seconds"`
	OperatingSystem    string              `json:"operating_system"`
	GoVersion          string              `json:"go_version"`
	PostgresVersion    string              `json:"postgresql_version"`
	TemporalVersion    string              `json:"temporal_version"`
	LogicalCPUs        int                 `json:"logical_cpus"`
	GoHeapSystemByte   uint64              `json:"go_heap_system_bytes"`
	Concurrency        int                 `json:"concurrency"`
	RequestLimit       int                 `json:"configured_request_limit"`
	DurationLimitSec   float64             `json:"configured_duration_limit_seconds"`
	TotalRequests      int                 `json:"total_requests"`
	Successful         int                 `json:"successful_requests"`
	Failed             int                 `json:"failed_requests"`
	RequestsPerSec     float64             `json:"requests_per_second"`
	Latency            Latency             `json:"api_latency"`
	Submission         Latency             `json:"payment_submission_api_latency"`
	Operations         map[string]int      `json:"operations"`
	ErrorCategories    map[string]int      `json:"error_categories"`
	Completion         AsyncTiming         `json:"payment_completion_latency"`
	Dispatch           AsyncTiming         `json:"workflow_dispatch_latency"`
	Reconciliation     AsyncTiming         `json:"reconciliation_latency"`
	Terminal           AsyncTiming         `json:"submission_to_terminal_latency"`
	ProcessingWindow   AsyncTiming         `json:"processing_start_to_terminal_latency"`
	Workflow           AsyncTiming         `json:"temporal_workflow_execution_latency"`
	WorkflowCandidates int                 `json:"temporal_workflow_sample_candidates"`
	Invariant          verification.Report `json:"financial_invariants"`
	PendingSubmitted   int                 `json:"pending_submitted_payments"`
	Recovery           string              `json:"recovery_result"`
	Limitations        []string            `json:"known_limitations"`
	Passed             bool                `json:"passed"`
}

func latency(samples []time.Duration) Latency {
	if len(samples) == 0 {
		return Latency{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum time.Duration
	for _, sample := range samples {
		sum += sample
	}
	value := func(i int) float64 { return float64(samples[i]) / float64(time.Millisecond) }
	index := func(p float64) int { return int(math.Ceil(float64(len(samples))*p)) - 1 }
	return Latency{
		AverageMS: float64(sum) / float64(len(samples)) / float64(time.Millisecond),
		MedianMS:  value(index(0.50)), P95MS: value(index(0.95)), P99MS: value(index(0.99)),
		MaximumMS: value(len(samples) - 1),
	}
}

func (r *Result) runtimeMetadata() {
	r.Label = label
	r.OperatingSystem = runtime.GOOS + "/" + runtime.GOARCH
	r.GoVersion = runtime.Version()
	r.LogicalCPUs = runtime.NumCPU()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	r.GoHeapSystemByte = memory.HeapSys
}

func WriteResult(r Result, format, directory string) ([]string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	base := filepath.Join(directory, "payflow-"+r.DateUTC.UTC().Format("20060102T150405.000000000Z"))
	var written []string
	write := func(extension string, body []byte) error {
		name := base + extension
		if err := os.WriteFile(name, body, 0o600); err != nil {
			return err
		}
		written = append(written, name)
		return nil
	}
	if format == "json" || format == "all" {
		body, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return written, err
		}
		if err := write(".json", append(body, '\n')); err != nil {
			return written, err
		}
	}
	if format == "csv" || format == "all" {
		var out strings.Builder
		csvWriter := csv.NewWriter(&out)
		errorsJSON, _ := json.Marshal(r.ErrorCategories)
		limitationsJSON, _ := json.Marshal(r.Limitations)
		headers := []string{"label", "test_name", "date_utc", "duration_seconds", "warmup_seconds", "operating_system", "go_version", "postgresql_version", "temporal_version", "logical_cpus", "go_heap_system_bytes", "concurrency", "configured_request_limit", "configured_duration_limit_seconds", "total_requests", "successful", "failed", "requests_per_second", "average_ms", "median_ms", "p95_ms", "p99_ms", "maximum_ms", "submission_p50_ms", "submission_p95_ms", "submission_p99_ms", "completion_count", "completion_average_ms", "dispatch_count", "dispatch_average_ms", "reconciliation_count", "reconciliation_average_ms", "terminal_count", "terminal_average_ms", "processing_window_count", "processing_window_average_ms", "workflow_sample_candidates", "workflow_count", "workflow_average_ms", "pending_submitted", "invariant_passed", "invariant_violations", "approval_journals", "expected_merchant_payable", "actual_merchant_payable", "error_categories_json", "recovery_result", "known_limitations_json", "passed"}
		row := []string{r.Label, r.TestName, r.DateUTC.Format(time.RFC3339Nano), decimal(r.DurationSeconds), decimal(r.WarmupSeconds), r.OperatingSystem, r.GoVersion, r.PostgresVersion, r.TemporalVersion, strconv.Itoa(r.LogicalCPUs), strconv.FormatUint(r.GoHeapSystemByte, 10), strconv.Itoa(r.Concurrency), strconv.Itoa(r.RequestLimit), decimal(r.DurationLimitSec), strconv.Itoa(r.TotalRequests), strconv.Itoa(r.Successful), strconv.Itoa(r.Failed), decimal(r.RequestsPerSec), decimal(r.Latency.AverageMS), decimal(r.Latency.MedianMS), decimal(r.Latency.P95MS), decimal(r.Latency.P99MS), decimal(r.Latency.MaximumMS), decimal(r.Submission.MedianMS), decimal(r.Submission.P95MS), decimal(r.Submission.P99MS), strconv.Itoa(r.Completion.Count), decimal(r.Completion.AverageMS), strconv.Itoa(r.Dispatch.Count), decimal(r.Dispatch.AverageMS), strconv.Itoa(r.Reconciliation.Count), decimal(r.Reconciliation.AverageMS), strconv.Itoa(r.Terminal.Count), decimal(r.Terminal.AverageMS), strconv.Itoa(r.ProcessingWindow.Count), decimal(r.ProcessingWindow.AverageMS), strconv.Itoa(r.WorkflowCandidates), strconv.Itoa(r.Workflow.Count), decimal(r.Workflow.AverageMS), strconv.Itoa(r.PendingSubmitted), strconv.FormatBool(r.Invariant.Passed), strconv.Itoa(r.Invariant.ViolationCount), strconv.Itoa(r.Invariant.Journals), strconv.FormatInt(r.Invariant.ExpectedPayable, 10), strconv.FormatInt(r.Invariant.ActualPayable, 10), string(errorsJSON), r.Recovery, string(limitationsJSON), strconv.FormatBool(r.Passed)}
		if err := csvWriter.Write(headers); err != nil {
			return written, err
		}
		if err := csvWriter.Write(row); err != nil {
			return written, err
		}
		csvWriter.Flush()
		if err := csvWriter.Error(); err != nil {
			return written, err
		}
		if err := write(".csv", []byte(out.String())); err != nil {
			return written, err
		}
	}
	if format == "markdown" || format == "all" {
		if err := write(".md", []byte(Markdown(r))); err != nil {
			return written, err
		}
	}
	return written, nil
}

func decimal(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func Markdown(r Result) string {
	status := "PASS"
	if !r.Passed {
		status = "FAIL OR INCOMPLETE"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n**%s** — %s, %s UTC\n\n", label, r.TestName, status, r.DateUTC.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Metric | Observed |\n| --- | ---: |\n")
	for _, row := range [][2]string{
		{"Duration / warmup (s)", decimal(r.DurationSeconds) + " / " + decimal(r.WarmupSeconds)}, {"Concurrency", strconv.Itoa(r.Concurrency)},
		{"Configured request / duration limits", fmt.Sprintf("%d / %s s", r.RequestLimit, decimal(r.DurationLimitSec))},
		{"Requests / success / failure", fmt.Sprintf("%d / %d / %d", r.TotalRequests, r.Successful, r.Failed)},
		{"Requests/s", decimal(r.RequestsPerSec)},
		{"API latency avg / median / p95 / p99 / max (ms)", fmt.Sprintf("%s / %s / %s / %s / %s", decimal(r.Latency.AverageMS), decimal(r.Latency.MedianMS), decimal(r.Latency.P95MS), decimal(r.Latency.P99MS), decimal(r.Latency.MaximumMS))},
		{"Submission API p50 / p95 / p99 (ms)", fmt.Sprintf("%s / %s / %s", decimal(r.Submission.MedianMS), decimal(r.Submission.P95MS), decimal(r.Submission.P99MS))},
		{"Completion avg (ms, count)", fmt.Sprintf("%s (%d)", decimal(r.Completion.AverageMS), r.Completion.Count)},
		{"Dispatch avg (ms, count)", fmt.Sprintf("%s (%d)", decimal(r.Dispatch.AverageMS), r.Dispatch.Count)},
		{"Reconciliation avg (ms, count)", fmt.Sprintf("%s (%d)", decimal(r.Reconciliation.AverageMS), r.Reconciliation.Count)},
		{"Submission to terminal avg (ms, count)", fmt.Sprintf("%s (%d)", decimal(r.Terminal.AverageMS), r.Terminal.Count)},
		{"Processing start to terminal avg (ms, count)", fmt.Sprintf("%s (%d)", decimal(r.ProcessingWindow.AverageMS), r.ProcessingWindow.Count)},
		{"Temporal workflow avg (ms, observed / sampled)", fmt.Sprintf("%s (%d / %d)", decimal(r.Workflow.AverageMS), r.Workflow.Count, r.WorkflowCandidates)},
		{"Pending submitted payments", strconv.Itoa(r.PendingSubmitted)},
		{"Financial invariant violations", strconv.Itoa(r.Invariant.ViolationCount)},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", row[0], row[1])
	}
	fmt.Fprintf(&b, "\nEnvironment: %s; %s; PostgreSQL %s; Temporal %s; %d logical CPUs; Go heap system %d bytes.\n\n", r.OperatingSystem, r.GoVersion, r.PostgresVersion, r.TemporalVersion, r.LogicalCPUs, r.GoHeapSystemByte)
	fmt.Fprintf(&b, "Recovery: %s. Temporal history is sampled from at most 100 terminal dispatches. Database operation and ledger posting latencies are not individually instrumented.\n\n", r.Recovery)
	if len(r.ErrorCategories) > 0 {
		b.WriteString("Errors by ConnectRPC category:\n\n")
		keys := make([]string, 0, len(r.ErrorCategories))
		for key := range r.ErrorCategories {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "- `%s`: %d\n", key, r.ErrorCategories[key])
		}
		b.WriteByte('\n')
	}
	if len(r.Invariant.Violations) > 0 {
		b.WriteString("Invariant violations (first 100):\n\n")
		for _, issue := range r.Invariant.Violations {
			fmt.Fprintf(&b, "- %s\n", issue)
		}
		b.WriteByte('\n')
	}
	for _, limitation := range r.Limitations {
		fmt.Fprintf(&b, "- Limitation: %s\n", limitation)
	}
	return b.String()
}
