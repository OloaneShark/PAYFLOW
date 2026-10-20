package loadtest

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func safeConfig() Config {
	return Config{
		Preset: "smoke", Workload: "mixed", API: "http://127.0.0.1:18080",
		DatabaseURL: "postgres://payflow:local@127.0.0.1:15432/payflow_loadtest_smoke?sslmode=disable",
		Environment: "development", AllowLocalTest: true,
	}
}

func TestDedicatedLocalSafety(t *testing.T) {
	c := safeConfig()
	if err := c.Defaults(); err != nil || c.Requests != 20 || c.Clients != 2 {
		t.Fatalf("safe defaults: %+v %v", c, err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.AllowLocalTest = false },
		func(c *Config) { c.Environment = "production" },
		func(c *Config) { c.API = "https://payments.example.com" },
		func(c *Config) { c.API = "http://0.0.0.0:18080" },
		func(c *Config) { c.API = "http://127.0.0.1:18080/other" },
		func(c *Config) { c.DatabaseURL = "postgres://payflow:local@127.0.0.1:15432/payflow?sslmode=disable" },
		func(c *Config) {
			c.DatabaseURL = "postgres://payflow:local@127.0.0.1:5432/payflow_loadtest_smoke?sslmode=disable"
		},
		func(c *Config) {
			c.DatabaseURL = "postgres://payflow:local@db.example.com:15432/payflow_loadtest_smoke?sslmode=disable"
		},
		func(c *Config) { c.Clients = 1000 },
		func(c *Config) { c.Requests = 5001 },
		func(c *Config) { c.Requests = 1001 },
		func(c *Config) { c.Clients = 17 },
		func(c *Config) { c.Duration = 6 * time.Minute },
		func(c *Config) { c.Preset = "stress" },
		func(c *Config) { c.Preset = "sustained" },
		func(c *Config) { c.Warmup = 2 * time.Second },
		func(c *Config) { c.Trials = 2 },
		func(c *Config) { c.Trials = 6; c.StressOptIn = true },
	} {
		bad := c
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("unsafe configuration accepted: %+v", bad)
		}
	}
}

func TestSustainedPresetAndTrialComparison(t *testing.T) {
	c := safeConfig()
	c.Preset, c.StressOptIn = "sustained", true
	if err := c.Defaults(); err != nil || c.Requests != 5000 || c.Clients != 8 || c.Duration != 2*time.Minute || c.Warmup != 10*time.Second {
		t.Fatalf("sustained defaults: %+v %v", c, err)
	}
	a := Result{TestName: "baseline/mixed", Concurrency: 8, TotalRequests: 200, RequestsPerSec: 100, Passed: true}
	b := a
	b.RequestsPerSec = 120
	summary, err := SummarizeTrials([]Result{a, b})
	if err != nil || !summary.Passed || summary.ThroughputMin != 100 || summary.ThroughputMedian != 110 || summary.ThroughputMax != 120 || summary.TrialCount != 2 {
		t.Fatalf("trial summary: %+v %v", summary, err)
	}
	comparison, err := CompareResults(a, b)
	if err != nil || math.Abs(comparison.ThroughputDelta-20) > 1e-9 {
		t.Fatalf("comparison: %+v %v", comparison, err)
	}
	b.Concurrency = 4
	if _, err := CompareResults(a, b); err == nil {
		t.Fatal("accepted incomparable runs")
	}
	old := safeConfig()
	old.Preset = "baseline"
	if err := old.Defaults(); err != nil || !ComparableToConfig(a, old) {
		t.Fatal("legacy single-run result should compare with matching configuration")
	}
	paths, err := WriteTrialSummary(summary, "all", t.TempDir())
	if err != nil || len(paths) != 3 {
		t.Fatalf("trial artifacts: %v %v", paths, err)
	}
}

func TestLatencyAndOutputsNeverContainCredentials(t *testing.T) {
	got := latency([]time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond})
	if got.AverageMS != 3 || got.MedianMS != 3 || got.P95MS != 5 || got.P99MS != 5 || got.MaximumMS != 5 {
		t.Fatalf("latency summary: %+v", got)
	}
	result := Result{TestName: "smoke/create", Latency: got}
	result.runtimeMetadata()
	markdown := Markdown(result)
	if !strings.Contains(markdown, label) || strings.Contains(markdown, "pfk_") || strings.Contains(markdown, "postgres://") {
		t.Fatal("unsafe or incomplete benchmark summary")
	}
	result.DateUTC = time.Now().UTC()
	result.PostgresVersion = "17.6"
	result.TemporalVersion = "unavailable"
	paths, err := WriteResult(result, "all", t.TempDir())
	if err != nil || len(paths) != 3 {
		t.Fatalf("artifacts: %v %v", paths, err)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(body), "pfk_") || strings.Contains(string(body), "postgres://") {
			t.Fatalf("artifact leaked a credential: %s %v", path, err)
		}
		if filepath.Ext(path) == ".csv" {
			rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
			if err != nil || len(rows) != 2 || len(rows[0]) != len(rows[1]) || !strings.Contains(strings.Join(rows[0], ","), "postgresql_version") {
				t.Fatalf("incomplete CSV: %v %v", rows, err)
			}
		}
	}
}
