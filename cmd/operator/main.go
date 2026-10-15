package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/operator"
	"payflow/internal/payment"
)

type result struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	data, err := run()
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(result{Data: data, Error: err.Error()})
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result{OK: true, Data: data})
}

func run() (any, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	host := config.ConnConfig.Host
	if host != "localhost" && !(net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return nil, errors.New("operator command requires a loopback PostgreSQL address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	svc := operator.New(pool)
	args := os.Args[1:]
	if len(args) == 0 {
		return nil, usage()
	}
	switch args[0] {
	case "inspect":
		if len(args) != 2 {
			return nil, usage()
		}
		return svc.Inspect(ctx, args[1])
	case "actions":
		if len(args) != 2 {
			return nil, usage()
		}
		return svc.ActionsForPayment(ctx, args[1])
	case "stuck":
		if len(args) != 1 {
			return nil, usage()
		}
		return svc.Candidates(ctx, payment.StatusProcessing, 100)
	case "unresolved":
		if len(args) != 1 {
			return nil, usage()
		}
		return svc.Candidates(ctx, payment.StatusReconciliationRequired, 100)
	case "issues":
		if len(args) != 1 {
			return nil, usage()
		}
		return svc.Ledger.CompletedIssues(ctx, 1000)
	case "balance-provider":
		if len(args) != 2 {
			return nil, usage()
		}
		return svc.Ledger.Balance(ctx, "PROVIDER_RECEIVABLE", "", args[1])
	case "balance-merchant":
		if len(args) != 3 {
			return nil, usage()
		}
		return svc.Ledger.Balance(ctx, "MERCHANT_PAYABLE", args[1], args[2])
	case "recover":
		if len(args) != 3 || args[2] != "--confirm" {
			return nil, errors.New("recover requires a payment ID and --confirm")
		}
		return svc.Recover(ctx, args[1], actor())
	case "backfill":
		if len(args) != 2 || args[1] != "--confirm" {
			return nil, errors.New("backfill requires --confirm")
		}
		a, n, err := svc.Backfill(ctx, actor())
		return struct {
			Action   operator.Action `json:"action"`
			Examined int             `json:"payments_examined"`
		}{a, n}, err
	default:
		return nil, usage()
	}
}

func actor() string {
	if s := os.Getenv("USERNAME"); s != "" {
		return s
	}
	if s := os.Getenv("USER"); s != "" {
		return s
	}
	return "local-operator"
}

func usage() error {
	return fmt.Errorf("usage: operator inspect ID | actions ID | stuck | unresolved | issues | balance-provider CURRENCY | balance-merchant MERCHANT CURRENCY | recover ID --confirm | backfill --confirm")
}
