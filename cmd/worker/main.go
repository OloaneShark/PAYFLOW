package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
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
	config, err := orchestration.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
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
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	activities := &orchestration.Activities{Store: store, Processor: processor}

	var temporalClient client.Client
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		temporalClient, err = client.DialContext(connectCtx, client.Options{HostPort: config.Address, Namespace: config.Namespace})
		cancel()
		if err == nil {
			break
		}
		log.Printf("Temporal connection: %v; retrying", err)
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
		log.Fatal(err)
	}
	defer w.Stop()
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	log.Printf("worker listening on Temporal task queue %s", config.TaskQueue)
	for {
		scanCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := coordinator.DispatchPending(scanCtx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dispatch pending workflows: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
