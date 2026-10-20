// Command loadtest sends bounded, authenticated traffic to a dedicated local
// PAYFLOW simulation and validates persisted financial invariants afterward.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"payflow/internal/loadtest"
)

func main() {
	config := loadtest.Config{}
	var comparePath string
	flag.StringVar(&config.Preset, "preset", "smoke", "smoke, baseline, sustained, stress, or recovery")
	flag.StringVar(&config.Workload, "workload", "mixed", "create, get, submit, repeat, concurrent-submit, mixed, or balance")
	flag.IntVar(&config.Requests, "requests", 0, "maximum measured requests (preset default when zero)")
	flag.IntVar(&config.Clients, "clients", 0, "concurrent clients (preset default when zero)")
	flag.DurationVar(&config.Duration, "duration", 0, "optional measured duration cap (maximum five minutes)")
	flag.DurationVar(&config.Warmup, "warmup", 0, "unmeasured balance-query warmup (maximum 30 seconds; opt-in required)")
	flag.IntVar(&config.Trials, "trials", 1, "independent trials, 1 to 5 (multiple trials require opt-in)")
	flag.DurationVar(&config.RequestTimeout, "timeout", 5*time.Second, "per-request timeout")
	flag.DurationVar(&config.SettleTimeout, "settle-timeout", 45*time.Second, "maximum wait for submitted payments to reach a terminal state")
	flag.StringVar(&config.API, "api", "http://127.0.0.1:18080", "dedicated loopback API origin")
	flag.StringVar(&config.OutputFormat, "format", "all", "json, csv, markdown, or all")
	flag.StringVar(&config.OutputDir, "output-dir", "results/loadtest", "artifact directory (ignored by Git)")
	flag.StringVar(&config.TemporalVersion, "temporal-version", "", "observed Temporal version, if independently available")
	flag.StringVar(&config.TemporalAddress, "temporal-address", "127.0.0.1:17233", "loopback Temporal gRPC target for bounded workflow timing sample")
	flag.StringVar(&config.TemporalNS, "temporal-namespace", "default", "local Temporal namespace for workflow timing sample")
	flag.StringVar(&config.RecoveryScenario, "recovery-scenario", "", "manual local fault label for recovery output")
	flag.StringVar(&comparePath, "compare", "", "prior single-trial PAYFLOW JSON result for descriptive comparison")
	flag.BoolVar(&config.AllowLocalTest, "allow-local-test", false, "explicitly allow a dedicated disposable local test")
	flag.BoolVar(&config.StressOptIn, "stress-opt-in", false, "explicitly allow the stress preset")
	flag.Parse()
	config.DatabaseURL = os.Getenv("PAYFLOW_LOADTEST_DATABASE_URL")
	config.Environment = os.Getenv("PAYFLOW_ENV")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := config.Defaults(); err != nil || (comparePath != "" && config.Trials != 1) {
		fmt.Fprintln(os.Stderr, "invalid local benchmark configuration or comparison requires one trial")
		os.Exit(1)
	}
	var baseline loadtest.Result
	if comparePath != "" {
		var err error
		baseline, err = loadtest.ReadResult(comparePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "comparison baseline must be an existing PAYFLOW result JSON file")
			os.Exit(1)
		}
		if !loadtest.ComparableToConfig(baseline, config) {
			fmt.Fprintln(os.Stderr, "comparison requires matching workload, concurrency, request and duration limits, and warmup")
			os.Exit(1)
		}
	}
	results := make([]loadtest.Result, 0, config.Trials)
	for i := 0; i < config.Trials; i++ {
		result, err := loadtest.Run(ctx, config)
		if err != nil {
			// Database and transport errors may embed connection information.
			fmt.Fprintln(os.Stderr, "local load test failed; inspect the dedicated test services and options")
			os.Exit(1)
		}
		files, err := loadtest.WriteResult(result, config.OutputFormat, config.OutputDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "local load test could not write results")
			os.Exit(1)
		}
		for _, file := range files {
			fmt.Println(file)
		}
		results = append(results, result)
	}
	if config.Trials > 1 {
		summary, err := loadtest.SummarizeTrials(results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "could not summarize benchmark trials")
			os.Exit(1)
		}
		files, err := loadtest.WriteTrialSummary(summary, config.OutputFormat, config.OutputDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "could not write benchmark trial summary")
			os.Exit(1)
		}
		for _, file := range files {
			fmt.Println(file)
		}
	}
	if comparePath != "" {
		comparison, err := loadtest.CompareResults(baseline, results[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "comparison requires matching workload, concurrency, request and duration limits, and warmup")
			os.Exit(1)
		}
		body, _ := json.MarshalIndent(comparison, "", "  ")
		fmt.Println(string(body))
	}
	for _, result := range results {
		if result.Passed {
			continue
		}
		fmt.Fprintln(os.Stderr, "local load test reported request failures, unfinished submissions, or financial invariant violations")
		os.Exit(1)
	}
}
