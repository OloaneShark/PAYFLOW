// Command credentials is a local-only provisioning tool. The key appears once
// on stdout; redirect it to a private location and never commit it.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) != 2 || (args[0] != "issue" && args[0] != "rotate" && args[0] != "revoke") {
		return fmt.Errorf("usage: credentials issue MERCHANT | rotate KEY_ID | revoke KEY_ID")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("invalid DATABASE_URL")
	}
	host := config.ConnConfig.Host
	if host != "localhost" && !(net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("credential provisioning requires a loopback PostgreSQL address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := &auth.Store{Pool: pool}
	switch args[0] {
	case "issue":
		id, key, err := store.Issue(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("key_id=%s\napi_key=%s\n", id, key)
	case "rotate":
		id, key, err := store.Rotate(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("key_id=%s\napi_key=%s\n", id, key)
	case "revoke":
		if err := store.Revoke(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("revoked")
	}
	return nil
}
