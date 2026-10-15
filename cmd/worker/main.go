package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"payflow/internal/banking"
	"payflow/internal/observability"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/recovery"
	"payflow/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	config, err := orchestration.ConfigFromEnv()
	if err != nil {
		logger.Error("invalid Temporal configuration", "error_category", "configuration")
		os.Exit(1)
	}
	recoveryInterval := 10 * time.Second
	if v := os.Getenv("PAYFLOW_RECOVERY_INTERVAL"); v != "" {
		recoveryInterval, err = time.ParseDuration(v)
		if err != nil || recoveryInterval <= 0 || recoveryInterval > 5*time.Minute {
			logger.Error("PAYFLOW_RECOVERY_INTERVAL must be positive and at most five minutes")
			os.Exit(1)
		}
	}
	recoveryMaxInterval := 5 * time.Minute
	if v := os.Getenv("PAYFLOW_RECOVERY_MAX_INTERVAL"); v != "" {
		recoveryMaxInterval, err = time.ParseDuration(v)
		if err != nil || recoveryMaxInterval < recoveryInterval || recoveryMaxInterval > time.Hour {
			logger.Error("PAYFLOW_RECOVERY_MAX_INTERVAL must be at least the interval and at most one hour")
			os.Exit(1)
		}
	}
	maxAttempts := 8
	if v := os.Getenv("PAYFLOW_RECOVERY_MAX_ATTEMPTS"); v != "" {
		maxAttempts, err = strconv.Atoi(v)
		if err != nil || maxAttempts < 1 || maxAttempts > 100 {
			logger.Error("PAYFLOW_RECOVERY_MAX_ATTEMPTS must be between 1 and 100")
			os.Exit(1)
		}
	}
	metricsAddr := os.Getenv("PAYFLOW_WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = "127.0.0.1:9091"
	}
	if host, _, err := net.SplitHostPort(metricsAddr); err != nil || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		logger.Error("PAYFLOW_WORKER_METRICS_ADDR must be loopback")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		logger.Error("database pool failed", "error_category", "database")
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		logger.Error("database unavailable", "error_category", "database")
		os.Exit(1)
	}
	shutdownTelemetry := observability.ConfigureOpenTelemetry()
	defer shutdownTelemetry(context.Background())
	metrics := observability.New()
	store := &storage.Postgres{Pool: pool, Metrics: metrics}
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool, Metrics: metrics})
	processor.Metrics = metrics
	activities := &orchestration.Activities{Store: store, Processor: processor}
	var started atomic.Bool
	var temporalClient client.Client
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", observability.Liveness)
	healthMux.HandleFunc("/readyz", observability.Readiness(
		func(context.Context) error {
			if !started.Load() {
				return errors.New("worker starting")
			}
			return nil
		},
		func(ctx context.Context) error {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			err := pool.Ping(checkCtx)
			if err != nil {
				metrics.Inc("payflow_database_errors_total")
			}
			return err
		},
		func(ctx context.Context) error {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_, err := temporalClient.CheckHealth(checkCtx, nil)
			return err
		},
	))
	healthMux.Handle("/metrics", metrics)
	healthServer := &http.Server{Addr: metricsAddr, Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker status server failed", "error_category", "http")
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdownCtx)
	}()

	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		temporalClient, err = client.DialContext(connectCtx, client.Options{HostPort: config.Address, Namespace: config.Namespace})
		cancel()
		if err == nil {
			break
		}
		logger.Warn("Temporal connection failed; retrying", "error_category", "temporal")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, config.TaskQueue, worker.Options{WorkerStopTimeout: 15 * time.Second})
	w.RegisterWorkflow(orchestration.PaymentWorkflow)
	w.RegisterActivityWithOptions(activities.Advance, activity.RegisterOptions{Name: orchestration.AdvanceActivityName})
	if err := w.Start(); err != nil {
		logger.Error("worker start failed", "error_category", "temporal")
		os.Exit(1)
	}
	defer w.Stop()
	started.Store(true)
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}, Metrics: metrics}
	go (&recovery.Scanner{Pool: pool, Processor: processor, Logger: logger, Interval: recoveryInterval, MaxInterval: recoveryMaxInterval, MaxAttempts: maxAttempts, BatchSize: 20, Metrics: metrics}).Run(ctx)
	logger.Info("worker listening", "task_queue", config.TaskQueue)
	for {
		scanCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := coordinator.DispatchPending(scanCtx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("dispatch scan failed", "error_category", "dispatch")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
