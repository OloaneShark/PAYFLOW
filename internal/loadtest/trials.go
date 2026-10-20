package loadtest

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TrialSummary retains independent observations; percentiles are never pooled
// across trials with different sample counts or durations.
type TrialSummary struct {
	Label                string    `json:"label"`
	TestName             string    `json:"test_name"`
	DateUTC              time.Time `json:"date_utc"`
	Concurrency          int       `json:"concurrency"`
	TrialCount           int       `json:"trial_count"`
	ThroughputsPerSecond []float64 `json:"throughputs_per_second"`
	TrialPassed          []bool    `json:"trial_passed"`
	ThroughputMin        float64   `json:"throughput_min"`
	ThroughputMedian     float64   `json:"throughput_median"`
	ThroughputP95        float64   `json:"throughput_p95"`
	ThroughputMax        float64   `json:"throughput_max"`
	FailedRequests       int       `json:"failed_requests"`
	PendingSubmitted     int       `json:"pending_submitted_payments"`
	InvariantViolations  int       `json:"invariant_violations"`
	Passed               bool      `json:"passed"`
}

func SummarizeTrials(results []Result) (TrialSummary, error) {
	if len(results) == 0 {
		return TrialSummary{}, errors.New("no benchmark trials")
	}
	first := results[0]
	s := TrialSummary{Label: label, TestName: first.TestName, DateUTC: time.Now().UTC(),
		Concurrency: first.Concurrency, TrialCount: len(results), Passed: true}
	for _, r := range results {
		if r.TestName != first.TestName || r.Concurrency != first.Concurrency {
			return TrialSummary{}, errors.New("trials have different workloads or concurrency")
		}
		s.ThroughputsPerSecond = append(s.ThroughputsPerSecond, r.RequestsPerSec)
		s.TrialPassed = append(s.TrialPassed, r.Passed)
		s.FailedRequests += r.Failed
		s.PendingSubmitted += r.PendingSubmitted
		s.InvariantViolations += r.Invariant.ViolationCount
		s.Passed = s.Passed && r.Passed
	}
	values := append([]float64(nil), s.ThroughputsPerSecond...)
	sort.Float64s(values)
	s.ThroughputMin = values[0]
	if len(values)%2 == 0 {
		s.ThroughputMedian = (values[len(values)/2-1] + values[len(values)/2]) / 2
	} else {
		s.ThroughputMedian = values[len(values)/2]
	}
	s.ThroughputP95 = values[percentileIndex(len(values), .95)]
	s.ThroughputMax = values[len(values)-1]
	return s, nil
}

func percentileIndex(n int, p float64) int {
	i := int(math.Ceil(float64(n)*p)) - 1
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func WriteTrialSummary(s TrialSummary, format, directory string) ([]string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	base := filepath.Join(directory, "payflow-trials-"+s.DateUTC.UTC().Format("20060102T150405.000000000Z"))
	var files []string
	write := func(ext string, body []byte) error {
		path := base + ext
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return err
		}
		files = append(files, path)
		return nil
	}
	if format == "json" || format == "all" {
		body, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return files, err
		}
		if err := write(".json", append(body, '\n')); err != nil {
			return files, err
		}
	}
	if format == "csv" || format == "all" {
		var b strings.Builder
		w := csv.NewWriter(&b)
		_ = w.Write([]string{"trial", "requests_per_second", "test_name", "concurrency", "passed"})
		for i, value := range s.ThroughputsPerSecond {
			_ = w.Write([]string{strconv.Itoa(i + 1), decimal(value), s.TestName, strconv.Itoa(s.Concurrency), strconv.FormatBool(s.TrialPassed[i])})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return files, err
		}
		if err := write(".csv", []byte(b.String())); err != nil {
			return files, err
		}
	}
	if format == "markdown" || format == "all" {
		var b strings.Builder
		fmt.Fprintf(&b, "# %s: repeated trials\n\n%s; %d independent trials; concurrency %d.\n\n", s.Label, s.TestName, s.TrialCount, s.Concurrency)
		fmt.Fprintf(&b, "Throughput min / median / p95 / max: %.3f / %.3f / %.3f / %.3f requests/s.\n\n", s.ThroughputMin, s.ThroughputMedian, s.ThroughputP95, s.ThroughputMax)
		fmt.Fprintf(&b, "Failures: %d requests, %d pending submissions, %d financial invariant violations. Overall passed: %t.\n\n", s.FailedRequests, s.PendingSubmitted, s.InvariantViolations, s.Passed)
		b.WriteString("Individual trial JSON files contain API latency percentiles and asynchronous timing. Trial summaries do not imply production capacity.\n")
		if err := write(".md", []byte(b.String())); err != nil {
			return files, err
		}
	}
	return files, nil
}

type Comparison struct {
	Label           string  `json:"label"`
	Baseline        string  `json:"baseline_test_name"`
	Current         string  `json:"current_test_name"`
	Concurrency     int     `json:"concurrency"`
	BaselineRPS     float64 `json:"baseline_requests_per_second"`
	CurrentRPS      float64 `json:"current_requests_per_second"`
	ThroughputDelta float64 `json:"throughput_delta_percent"`
	BaselineP50MS   float64 `json:"baseline_p50_ms"`
	CurrentP50MS    float64 `json:"current_p50_ms"`
	BaselineP95MS   float64 `json:"baseline_p95_ms"`
	CurrentP95MS    float64 `json:"current_p95_ms"`
	BaselineP99MS   float64 `json:"baseline_p99_ms"`
	CurrentP99MS    float64 `json:"current_p99_ms"`
	Caveat          string  `json:"caveat"`
}

func CompareResults(baseline, current Result) (Comparison, error) {
	if baseline.TestName != current.TestName || baseline.Concurrency != current.Concurrency ||
		comparisonRequestLimit(baseline) != comparisonRequestLimit(current) || baseline.DurationLimitSec != current.DurationLimitSec ||
		baseline.WarmupSeconds != current.WarmupSeconds {
		return Comparison{}, errors.New("baseline and current workload, concurrency, request and duration limits, and warmup must match")
	}
	if baseline.RequestsPerSec <= 0 {
		return Comparison{}, errors.New("baseline has no measured throughput")
	}
	return Comparison{Label: label, Baseline: baseline.TestName, Current: current.TestName,
		Concurrency: current.Concurrency, BaselineRPS: baseline.RequestsPerSec, CurrentRPS: current.RequestsPerSec,
		ThroughputDelta: (current.RequestsPerSec/baseline.RequestsPerSec - 1) * 100,
		BaselineP50MS:   baseline.Latency.MedianMS, CurrentP50MS: current.Latency.MedianMS,
		BaselineP95MS: baseline.Latency.P95MS, CurrentP95MS: current.Latency.P95MS,
		BaselineP99MS: baseline.Latency.P99MS, CurrentP99MS: current.Latency.P99MS,
		Caveat: "Descriptive local comparison only; control host load, database state, duration, and sample size before interpreting differences."}, nil
}

// Older result files recorded the observed request count but not the limit.
func comparisonRequestLimit(r Result) int {
	if r.RequestLimit > 0 {
		return r.RequestLimit
	}
	return r.TotalRequests
}

func ComparableToConfig(baseline Result, config Config) bool {
	return baseline.TestName == config.Preset+"/"+config.Workload && baseline.Concurrency == config.Clients &&
		comparisonRequestLimit(baseline) == config.Requests && baseline.DurationLimitSec == config.Duration.Seconds() &&
		baseline.WarmupSeconds == config.Warmup.Seconds()
}

func ReadResult(path string) (Result, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var r Result
	if err := json.Unmarshal(body, &r); err != nil || r.Label != label {
		return Result{}, errors.New("comparison requires a PAYFLOW benchmark result JSON file")
	}
	return r, nil
}
