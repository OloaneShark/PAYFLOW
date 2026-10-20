// Command loadtest sends bounded, authenticated traffic to a dedicated local
// PAYFLOW simulation and validates persisted financial invariants afterward.
package main

import (
	"context"
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
	flag.StringVar(&config.Preset, "preset", "smoke", "smoke, baseline, stress, or recovery")
	flag.StringVar(&config.Workload, "workload", "mixed", "create, get, submit, repeat, concurrent-submit, mixed, or balance")
	flag.IntVar(&config.Requests, "requests", 0, "maximum measured requests (preset default when zero)")
	flag.IntVar(&config.Clients, "clients", 0, "concurrent clients (preset default when zero)")
	flag.DurationVar(&config.Duration, "duration", 0, "optional measured duration cap (maximum five minutes)")
	flag.DurationVar(&config.RequestTimeout, "timeout", 5*time.Second, "per-request timeout")
	flag.DurationVar(&config.SettleTimeout, "settle-timeout", 45*time.Second, "maximum wait for submitted payments to reach a terminal state")
	flag.StringVar(&config.API, "api", "http://127.0.0.1:18080", "dedicated loopback API origin")
	flag.StringVar(&config.OutputFormat, "format", "all", "json, csv, markdown, or all")
	flag.StringVar(&config.OutputDir, "output-dir", "results/loadtest", "artifact directory (ignored by Git)")
	flag.StringVar(&config.TemporalVersion, "temporal-version", "", "observed Temporal version, if independently available")
	flag.StringVar(&config.RecoveryScenario, "recovery-scenario", "", "manual local fault label for recovery output")
	flag.BoolVar(&config.AllowLocalTest, "allow-local-test", false, "explicitly allow a dedicated disposable local test")
	flag.BoolVar(&config.StressOptIn, "stress-opt-in", false, "explicitly allow the stress preset")
	flag.Parse()
	config.DatabaseURL = os.Getenv("PAYFLOW_LOADTEST_DATABASE_URL")
	config.Environment = os.Getenv("PAYFLOW_ENV")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	if !result.Passed {
		fmt.Fprintln(os.Stderr, "local load test reported request failures, unfinished submissions, or financial invariant violations")
		os.Exit(1)
	}
}
