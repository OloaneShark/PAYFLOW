package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/banking"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is required")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	devProcessing := os.Getenv("PAYFLOW_ENABLE_DEV_PROCESSING") == "true"
	if devProcessing {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			log.Fatal("development processing requires a loopback LISTEN_ADDR")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	store := &storage.Postgres{Pool: pool}
	service := payment.NewService(store)
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	var coordinator *orchestration.Coordinator
	if devProcessing {
		config, err := orchestration.ConfigFromEnv()
		if err != nil {
			log.Fatal(err)
		}
		coordinator = &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	}
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{Service: service, Processor: processor, Orchestrator: coordinator, DevProcessingEnabled: devProcessing})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{Addr: addr, Handler: mux, Protocols: protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("listening on %s", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
