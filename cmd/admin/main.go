// Command admin is a private, one-off ECS task. Its fixed command set does not
// execute shell input, and it never writes newly issued credentials to output.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/admin"
	"payflow/internal/config"
	"payflow/internal/operator"
	"payflow/internal/payment"
)

type command struct {
	action, arg, requestID string
	confirm                bool
}

func parse(args []string) (command, error) {
	if len(args) == 2 {
		switch args[0] {
		case "inspect", "actions":
			if payment.ValidPaymentID(args[1]) {
				return command{action: args[0], arg: args[1]}, nil
			}
		}
	}
	if len(args) == 1 {
		switch args[0] {
		case "stuck", "unresolved", "issues":
			return command{action: args[0]}, nil
		}
	}
	if len(args) == 2 && args[0] == "backfill" && args[1] == "--confirm" {
		return command{action: args[0], confirm: true}, nil
	}
	if len(args) == 3 && args[0] == "recover" && args[2] == "--confirm" && payment.ValidPaymentID(args[1]) {
		return command{action: args[0], arg: args[1], confirm: true}, nil
	}
	if len(args) == 4 && args[3] == "--confirm" && payment.ValidPaymentID(args[2]) {
		switch args[0] {
		case "issue":
			if args[1] != "" {
				return command{action: args[0], arg: args[1], requestID: args[2], confirm: true}, nil
			}
		case "rotate", "revoke":
			if payment.ValidPaymentID(args[1]) {
				return command{action: args[0], arg: args[1], requestID: args[2], confirm: true}, nil
			}
		}
	}
	return command{}, errors.New("invalid private administration command or missing --confirm")
}

func main() {
	result, err := run(os.Args[1:])
	if err != nil {
		// A backend error may contain sensitive connection information. Output a
		// fixed category; request IDs in the audit table identify the attempt.
		fmt.Fprintln(os.Stderr, `{"ok":false,"error":"private administration failed; review the request ID and task health"}`)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		OK   bool `json:"ok"`
		Data any  `json:"data"`
	}{true, result}); err != nil {
		os.Exit(1)
	}
}

func run(args []string) (any, error) {
	c, err := parse(args)
	if err != nil {
		return nil, err
	}
	if os.Getenv("PAYFLOW_ENV") != "production" || os.Getenv("PAYFLOW_ADMIN_ACTOR") == "" {
		return nil, errors.New("private administration task configuration is required")
	}
	dbConfig, err := config.Database()
	if err != nil {
		return nil, err
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(root, 3*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	actor := os.Getenv("PAYFLOW_ADMIN_ACTOR")
	op := operator.New(pool)
	creds := &admin.CredentialService{Pool: pool, Prefix: os.Getenv("PAYFLOW_CREDENTIAL_SECRET_PREFIX")}
	if c.action == "issue" || c.action == "rotate" {
		awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, errors.New("AWS credentials unavailable")
		}
		creds.Writer = &admin.SecretsManagerWriter{Client: secretsmanager.NewFromConfig(awsConfig)}
	}
	switch c.action {
	case "issue":
		return creds.Issue(ctx, c.arg, c.requestID, actor, c.confirm)
	case "rotate":
		return creds.Rotate(ctx, c.arg, c.requestID, actor, c.confirm)
	case "revoke":
		return creds.Revoke(ctx, c.arg, c.requestID, actor, c.confirm)
	case "inspect":
		return op.Inspect(ctx, c.arg)
	case "actions":
		return op.ActionsForPayment(ctx, c.arg)
	case "stuck":
		return op.Candidates(ctx, payment.StatusProcessing, 100)
	case "unresolved":
		return op.Candidates(ctx, payment.StatusReconciliationRequired, 100)
	case "issues":
		return op.Ledger.CompletedIssues(ctx, 1000)
	case "recover":
		return op.Recover(ctx, c.arg, actor)
	case "backfill":
		a, n, err := op.Backfill(ctx, actor)
		return struct {
			Action   operator.Action `json:"action"`
			Examined int             `json:"payments_examined"`
		}{a, n}, err
	default:
		return nil, errors.New("unsupported private administration command")
	}
}
