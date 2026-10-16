package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/db/migrations"
	"payflow/internal/config"
)

func main() {
	dbConfig, err := config.Database()
	if err != nil {
		log.Fatal("invalid database configuration")
	}
	requestTimeout := 5 * time.Minute
	if v := os.Getenv("PAYFLOW_MIGRATION_TIMEOUT"); v != "" {
		requestTimeout, err = time.ParseDuration(v)
		if err != nil || requestTimeout <= 0 || requestTimeout > 30*time.Minute {
			log.Fatal("PAYFLOW_MIGRATION_TIMEOUT must be positive and at most 30 minutes")
		}
	}
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(shutdownCtx, requestTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Apply(ctx, pool); err != nil {
		log.Fatal(err)
	}
	log.Println("migrations applied")
}
