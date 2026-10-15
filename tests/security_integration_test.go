package tests

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	paymentv1 "payflow/gen/go/payflow/v1"
	"payflow/internal/auth"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/observability"
	"payflow/internal/payment"
	"payflow/internal/recovery"
	"payflow/internal/storage"
)

func TestCredentialLifecycleAndTenantIsolation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	keys := &auth.Store{Pool: pool}
	merchantA := fmt.Sprintf("secure-a-%d", time.Now().UnixNano())
	merchantB := merchantA + "-b"
	id, key, err := keys.Issue(ctx, merchantA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "pfk_") || len(key) != 68 {
		t.Fatal("invalid key format")
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT encode(key_hash,'hex') FROM merchant_api_keys WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == key || strings.Contains(stored, key) || len(stored) != 64 {
		t.Fatal("plaintext credential stored")
	}
	who, err := keys.Authenticate(ctx, key)
	if err != nil || who != merchantA {
		t.Fatalf("valid key: %q %v", who, err)
	}
	if _, err := keys.Authenticate(ctx, "bad"); err != auth.ErrUnauthenticated {
		t.Fatalf("invalid key: %v", err)
	}
	nextID, nextKey, err := keys.Rotate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if nextID == id || nextKey == key {
		t.Fatal("rotation reused credential")
	}
	if _, err := keys.Authenticate(ctx, key); err != auth.ErrUnauthenticated {
		t.Fatalf("old key active: %v", err)
	}
	if who, err := keys.Authenticate(ctx, nextKey); err != nil || who != merchantA {
		t.Fatalf("rotated key: %q %v", who, err)
	}
	if err := keys.Revoke(ctx, nextID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, nextKey); err != auth.ErrUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}

	store := &storage.Postgres{Pool: pool}
	h := &payment.Handler{Service: payment.NewService(store), Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool}), Ledger: &ledger.Service{Pool: pool}, AuthRequired: true, DevProcessingEnabled: true}
	aCtx := auth.WithMerchant(ctx, merchantA)
	bCtx := auth.WithMerchant(ctx, merchantB)
	request := &paymentv1.CreatePaymentRequest{MerchantId: merchantA, IdempotencyKey: "secure", Amount: 125, Currency: "JPY"}
	if _, err := h.CreatePayment(ctx, request); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing auth: %v", err)
	}
	if _, err := h.CreatePayment(bCtx, request); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("spoofed merchant: %v", err)
	}
	created, err := h.CreatePayment(aCtx, request)
	if err != nil {
		t.Fatal(err)
	}
	paymentID := created.Payment.PaymentId
	if _, err := h.GetPayment(aCtx, &paymentv1.GetPaymentRequest{PaymentId: paymentID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetPayment(bCtx, &paymentv1.GetPaymentRequest{PaymentId: paymentID}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-merchant get: %v", err)
	}
	if _, err := h.SimulatePayment(bCtx, &paymentv1.SimulatePaymentRequest{PaymentId: paymentID, Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-merchant process: %v", err)
	}
	if _, err := h.ReconcilePayment(bCtx, &paymentv1.ReconcilePaymentRequest{PaymentId: paymentID}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-merchant reconcile: %v", err)
	}
	if p, err := h.Processor.Process(ctx, paymentID, banking.Approve); err != nil || p.Status != payment.StatusCompleted {
		t.Fatalf("process: %+v %v", p, err)
	}
	aBalance, err := h.GetMerchantBalance(aCtx, &paymentv1.GetMerchantBalanceRequest{Currency: "JPY"})
	if err != nil || aBalance.Amount != 125 {
		t.Fatalf("A balance: %+v %v", aBalance, err)
	}
	bBalance, err := h.GetMerchantBalance(bCtx, &paymentv1.GetMerchantBalanceRequest{Currency: "JPY"})
	if err != nil || bBalance.Amount != 0 {
		t.Fatalf("B balance: %+v %v", bBalance, err)
	}
}

func TestAutomaticRecoveryConcurrentAndDurable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := &storage.Postgres{Pool: pool}
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanner := &recovery.Scanner{Pool: pool, Processor: processor, Logger: logger, Interval: time.Millisecond, MaxAttempts: 3, BatchSize: 1000}
	approved := ledgerPayment(t, pool, fmt.Sprintf("recovery-approved-%d", time.Now().UnixNano()))
	claimed, ok, err := store.Claim(ctx, approved.ID, banking.Approve, approved.ID, "123e4567-e89b-42d3-a456-426614174024", time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := (&banking.Postgres{Pool: pool}).Execute(ctx, banking.Operation{ID: claimed.ProviderOperationID, PaymentID: claimed.ID, Scenario: banking.Approve}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, approved.ID); err != nil {
		t.Fatal(err)
	}
	declined := ledgerPayment(t, pool, fmt.Sprintf("recovery-declined-%d", time.Now().UnixNano()))
	c2, ok, err := store.Claim(ctx, declined.ID, banking.Decline, declined.ID, "123e4567-e89b-42d3-a456-426614174025", time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("decline claim: %v", err)
	}
	if _, err := (&banking.Postgres{Pool: pool}).Execute(ctx, banking.Operation{ID: c2.ProviderOperationID, PaymentID: c2.ID, Scenario: banking.Decline}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, declined.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := scanner.Scan(ctx); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := store.Get(ctx, approved.ID)
	if err != nil || p.Status != payment.StatusCompleted || journalCount(t, pool, approved.ID) != 1 {
		t.Fatalf("approval: %+v %v", p, err)
	}
	_, attempts, charges := providerFacts(t, pool, approved.ID)
	if attempts != 1 || charges != 1 {
		t.Fatalf("duplicate provider attempt: %d charges: %d", attempts, charges)
	}
	p, err = store.Get(ctx, declined.ID)
	if err != nil || p.Status != payment.StatusFailed || journalCount(t, pool, declined.ID) != 0 {
		t.Fatalf("decline: %+v %v", p, err)
	}
	// Reconstruct the service, as a restarted worker would, and verify that
	// terminal payments remain terminal and are not charged again.
	restarted := &recovery.Scanner{Pool: pool, Processor: payment.NewProcessor(&storage.Postgres{Pool: pool}, &banking.Postgres{Pool: pool}), Logger: logger, Interval: time.Millisecond, MaxAttempts: 3, BatchSize: 1000}
	if _, err := restarted.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if journalCount(t, pool, approved.ID) != 1 {
		t.Fatal("restart duplicated journal")
	}
}

func TestRecoveryUnknownOutcomeAndDatabaseReconnection(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	metrics := observability.New()
	store := &storage.Postgres{Pool: pool, Metrics: metrics}
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool, Metrics: metrics})
	processor.Metrics = metrics
	unknown := ledgerPayment(t, pool, fmt.Sprintf("unknown-recovery-%d", time.Now().UnixNano()))
	if p, err := processor.Process(ctx, unknown.ID, banking.NetworkTimeout); err != nil || p.Status != payment.StatusReconciliationRequired {
		t.Fatalf("initial timeout: %+v %v", p, err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanner := &recovery.Scanner{Pool: pool, Processor: processor, Logger: logger, Interval: time.Millisecond, MaxAttempts: 2, BatchSize: 1000, Metrics: metrics}
	for i := 0; i < 2; i++ {
		if _, err := scanner.Scan(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT recovery_attempts FROM payments WHERE id=$1`, unknown.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("unbounded or missing retries: %d", attempts)
	}
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := store.Get(ctx, unknown.ID)
	if err != nil || p.Status != payment.StatusReconciliationRequired || journalCount(t, pool, unknown.ID) != 0 {
		t.Fatalf("unknown became terminal: %+v %v", p, err)
	}
	_, _, charges := providerFacts(t, pool, unknown.ID)
	if charges != 0 {
		t.Fatal("unknown outcome charged")
	}
	metricResponse := httptest.NewRecorder()
	metrics.ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricResponse.Body.String(), "payflow_reconciliation_attempts_total ") || strings.Contains(metricResponse.Body.String(), "payflow_reconciliation_attempts_total 0\n") || strings.Contains(metricResponse.Body.String(), unknown.ID) {
		t.Fatalf("recovery metrics missing or high cardinality: %s", metricResponse.Body.String())
	}

	approved := ledgerPayment(t, pool, fmt.Sprintf("reconnect-recovery-%d", time.Now().UnixNano()))
	claimed, ok, err := store.Claim(ctx, approved.ID, banking.LostApprovalResponse, approved.ID, "123e4567-e89b-42d3-a456-426614174026", time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	if _, err := (&banking.Postgres{Pool: pool}).Execute(ctx, banking.Operation{ID: claimed.ProviderOperationID, PaymentID: claimed.ID, Scenario: banking.LostApprovalResponse}); err != banking.ErrLostResponse {
		t.Fatalf("lost approval: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, approved.ID); err != nil {
		t.Fatal(err)
	}
	badPool, err := pgxpool.New(ctx, "postgres://payflow:payflow@127.0.0.1:1/unreachable?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer badPool.Close()
	outage := &recovery.Scanner{Pool: badPool, Processor: processor, Logger: logger, Interval: time.Millisecond, MaxAttempts: 2, BatchSize: 10}
	failCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := outage.Scan(failCtx); err == nil {
		t.Fatal("unavailable database appeared healthy")
	}
	reconnected := &recovery.Scanner{Pool: pool, Processor: payment.NewProcessor(&storage.Postgres{Pool: pool}, &banking.Postgres{Pool: pool}), Logger: logger, Interval: time.Millisecond, MaxAttempts: 3, BatchSize: 1000}
	if _, err := reconnected.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	p, err = store.Get(ctx, approved.ID)
	if err != nil || p.Status != payment.StatusCompleted || journalCount(t, pool, approved.ID) != 1 {
		t.Fatalf("reconnect: %+v %v", p, err)
	}
	_, _, charges = providerFacts(t, pool, approved.ID)
	if charges != 1 {
		t.Fatalf("reconnect duplicate charges: %d", charges)
	}
}
