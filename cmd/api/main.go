package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/auth"
	"payflow/internal/banking"
	"payflow/internal/config"
	"payflow/internal/ledger"
	"payflow/internal/observability"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	devProcessing := os.Getenv("PAYFLOW_ENABLE_DEV_PROCESSING") == "true"
	if os.Getenv("PAYFLOW_ENV") == "production" {
		if addr != "0.0.0.0:8080" || os.Getenv("PAYFLOW_ENABLE_DEV_PROCESSING") == "true" {
			logger.Error("production requires LISTEN_ADDR=0.0.0.0:8080 and simulation disabled")
			os.Exit(1)
		}
	}
	if devProcessing {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			logger.Error("development processing requires a loopback LISTEN_ADDR")
			os.Exit(1)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	requestTimeout := 15 * time.Second
	if value := os.Getenv("PAYFLOW_REQUEST_TIMEOUT"); value != "" {
		var parseErr error
		requestTimeout, parseErr = time.ParseDuration(value)
		if parseErr != nil || requestTimeout <= 0 {
			logger.Error("PAYFLOW_REQUEST_TIMEOUT must be a positive duration")
			os.Exit(1)
		}
	}
	metricsAddr := os.Getenv("PAYFLOW_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = "127.0.0.1:9090"
	}
	if host, _, err := net.SplitHostPort(metricsAddr); err != nil || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		logger.Error("PAYFLOW_METRICS_ADDR must be loopback")
		os.Exit(1)
	}
	dbConfig, err := config.Database()
	if err != nil {
		logger.Error("invalid database configuration", "error_category", "configuration")
		os.Exit(1)
	}
	pool, err := pgxpool.NewWithConfig(ctx, dbConfig)
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
	service := payment.NewService(store)
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool, Metrics: metrics})
	processor.Metrics = metrics
	var coordinator *orchestration.Coordinator
	var temporalConfig *orchestration.Config
	if devProcessing {
		config, err := orchestration.ConfigFromEnv()
		if err != nil {
			logger.Error("invalid Temporal configuration", "error_category", "configuration")
			os.Exit(1)
		}
		coordinator = &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}, Metrics: metrics}
		temporalConfig = &config
	}
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{Service: service, Processor: processor, Orchestrator: coordinator, Submitter: &orchestration.SubmissionService{Store: store}, DevProcessingEnabled: devProcessing, AuthRequired: true, Ledger: &ledger.Service{Pool: pool}})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("/healthz", observability.Liveness)
	checks := []func(context.Context) error{func(ctx context.Context) error {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		err := pool.Ping(checkCtx)
		if err != nil {
			metrics.Inc("payflow_database_errors_total")
		}
		return err
	}}
	if temporalConfig != nil {
		checks = append(checks, func(ctx context.Context) error {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			c, err := client.DialContext(checkCtx, temporalConfig.ClientOptions())
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.CheckHealth(checkCtx, nil)
			return err
		})
	}
	mux.HandleFunc("/readyz", observability.Readiness(checks...))
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics)
	metricsServer := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error_category", "http")
		}
	}()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{Addr: addr, Handler: apiMiddleware(mux, &auth.Store{Pool: pool}, metrics, logger, requestTimeout), Protocols: protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("API shutdown failed", "error_category", "http")
		}
		_ = metricsServer.Shutdown(shutdownCtx)
	}()
	logger.Info("api listening", "address", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("api server failed", "error_category", "http")
		os.Exit(1)
	}
}
