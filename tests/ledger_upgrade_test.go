package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/auth"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/operator"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func phase3Schema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin := testPool(t)
	schema := fmt.Sprintf("upgrade_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	for _, name := range []string{"001_create_payments.up.sql", "002_simulated_processing.up.sql", "003_temporal_dispatch.up.sql"} {
		applyMigrationFile(t, pool, name)
	}
	return pool
}

func applyMigrationFile(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

func phase3Payment(t *testing.T, pool *pgxpool.Pool, scenario banking.Scenario, target string) payment.Payment {
	t.Helper()
	store := &storage.Postgres{Pool: pool}
	p, err := payment.NewService(store).Create(context.Background(), payment.CreateInput{
		MerchantID: fmt.Sprintf("historical-%d", time.Now().UnixNano()), IdempotencyKey: "original", Amount: 1200, Currency: "JPY",
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "123e4567-e89b-42d3-a456-426614174021"
	if _, ok, err := store.Claim(context.Background(), p.ID, scenario, p.ID, token, 30*time.Second); err != nil || !ok {
		t.Fatalf("Phase 3 claim: %v", err)
	}
	_, _ = (&banking.Postgres{Pool: pool}).Execute(context.Background(), banking.Operation{ID: p.ID, PaymentID: p.ID, Scenario: scenario})
	_, err = pool.Exec(context.Background(), `UPDATE payments SET status=$2,processing_token=NULL,processing_lease_until=NULL,
		completed_at=CASE WHEN $2='COMPLETED' THEN now() ELSE completed_at END,
		failed_at=CASE WHEN $2='FAILED' THEN now() ELSE failed_at END,
		reconciliation_required_at=CASE WHEN $2='RECONCILIATION_REQUIRED' THEN now() ELSE reconciliation_required_at END
		WHERE id=$1 AND processing_token=$3`, p.ID, target, token)
	if err != nil {
		t.Fatalf("Phase 3 finish %s: %v", target, err)
	}
	return p
}

func TestPhase3UpgradeBackfillsExactlyOnce(t *testing.T) {
	pool := phase3Schema(t)
	approved := phase3Payment(t, pool, banking.Approve, payment.StatusCompleted)
	declined := phase3Payment(t, pool, banking.Decline, payment.StatusFailed)
	uncertain := phase3Payment(t, pool, banking.NetworkTimeout, payment.StatusReconciliationRequired)
	applyMigrationFile(t, pool, "004_double_entry_ledger.up.sql")
	accounts := &ledger.Service{Pool: pool}
	issues, err := accounts.CompletedIssues(context.Background(), 100)
	if err != nil || len(issues) != 1 || issues[0].PaymentID != approved.ID || issues[0].Reason != "approval journal missing" {
		t.Fatalf("upgrade issues = %+v, %v", issues, err)
	}
	op := operator.New(pool)
	a, count, err := op.Backfill(context.Background(), "integration-test")
	if err != nil || count != 1 || a.State != "SUCCEEDED" {
		t.Fatalf("first backfill = %d, %v", count, err)
	}
	a, count, err = op.Backfill(context.Background(), "integration-test")
	if err != nil || count != 0 || a.State != "SUCCEEDED" {
		t.Fatalf("repeat backfill = %d, %v", count, err)
	}
	if journalCount(t, pool, approved.ID) != 1 || journalCount(t, pool, declined.ID) != 0 || journalCount(t, pool, uncertain.ID) != 0 {
		t.Fatal("upgrade changed non-approved payment journals")
	}
	issues, err = accounts.CompletedIssues(context.Background(), 100)
	if err != nil || len(issues) != 0 {
		t.Fatalf("post-backfill issues = %+v, %v", issues, err)
	}
	repeated, err := payment.NewService(&storage.Postgres{Pool: pool}).Create(context.Background(), payment.CreateInput{
		MerchantID: approved.MerchantID, IdempotencyKey: approved.IdempotencyKey, Amount: 1200, Currency: "JPY",
	})
	if err != nil || repeated.ID != approved.ID || repeated.Status != payment.StatusCompleted {
		t.Fatalf("historical idempotency = %+v,%v", repeated, err)
	}
}

func TestPhase4UpgradeThroughPrivateAdministrationPreservesPaymentJournalAndKey(t *testing.T) {
	pool := phase3Schema(t)
	applyMigrationFile(t, pool, "004_double_entry_ledger.up.sql")
	ctx := context.Background()
	store := &storage.Postgres{Pool: pool}
	created, err := payment.NewService(store).Create(ctx, payment.CreateInput{MerchantID: "upgrade-merchant", IdempotencyKey: "original", Amount: 400, Currency: "JPY"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := payment.NewProcessor(store, &banking.Postgres{Pool: pool}).Process(ctx, created.ID, banking.Approve); err != nil || p.Status != payment.StatusCompleted {
		t.Fatalf("complete before migration: %+v %v", p, err)
	}
	applyMigrationFile(t, pool, "005_merchant_credentials.up.sql")
	if journalCount(t, pool, created.ID) != 1 {
		t.Fatal("migration lost journal")
	}
	repeated, err := payment.NewService(store).Create(ctx, payment.CreateInput{MerchantID: created.MerchantID, IdempotencyKey: created.IdempotencyKey, Amount: created.Amount, Currency: created.Currency})
	if err != nil || repeated.ID != created.ID || repeated.Status != payment.StatusCompleted {
		t.Fatalf("migration changed idempotency: %+v %v", repeated, err)
	}
	keys := &auth.Store{Pool: pool}
	_, key, err := keys.Issue(ctx, created.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	applyMigrationFile(t, pool, "006_private_administration.up.sql")
	if merchant, err := keys.Authenticate(ctx, key); err != nil || merchant != created.MerchantID {
		t.Fatalf("upgraded auth: %q %v", merchant, err)
	}
	if journalCount(t, pool, created.ID) != 1 {
		t.Fatal("private administration migration lost approval journal")
	}
}

func TestPhase3UpgradeRejectsUnverifiedApproval(t *testing.T) {
	pool := phase3Schema(t)
	p := phase3Payment(t, pool, banking.Approve, payment.StatusCompleted)
	// Deliberately corrupt a pre-upgrade provider record to prove backfill does
	// not infer approval from the COMPLETED label alone.
	if _, err := pool.Exec(context.Background(), `UPDATE simulated_provider_operations
		SET outcome='PENDING',logical_charge_id=NULL WHERE payment_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	applyMigrationFile(t, pool, "004_double_entry_ledger.up.sql")
	accounts := &ledger.Service{Pool: pool}
	if _, err := accounts.Backfill(context.Background()); !errors.Is(err, ledger.ErrNotApproved) {
		t.Fatalf("inconsistent backfill error = %v", err)
	}
	if journalCount(t, pool, p.ID) != 0 {
		t.Fatal("fabricated approval journal")
	}
	issues, err := accounts.CompletedIssues(context.Background(), 100)
	if err != nil || len(issues) != 1 || issues[0].Reason != "provider approval missing or inconsistent" {
		t.Fatalf("inconsistent history issues = %+v,%v", issues, err)
	}
}

func TestPhase3UpgradeConcurrentPosting(t *testing.T) {
	pool := phase3Schema(t)
	p := phase3Payment(t, pool, banking.Approve, payment.StatusCompleted)
	applyMigrationFile(t, pool, "004_double_entry_ledger.up.sql")
	service := &ledger.Service{Pool: pool}
	const callers = 16
	var wg sync.WaitGroup
	ids := make(chan int64, callers)
	errorsCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := service.PostApproval(context.Background(), p.ID)
			if err != nil {
				errorsCh <- err
				return
			}
			ids <- j.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatalf("different journal IDs: %d and %d", first, id)
		}
	}
	if first == 0 || journalCount(t, pool, p.ID) != 1 {
		t.Fatal("concurrent posting did not produce exactly one journal")
	}
}
