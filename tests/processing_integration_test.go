package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	paymentv1 "payflow/gen/go/payflow/v1"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/banking"
	"payflow/internal/payment"
	"payflow/internal/storage"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/jackc/pgx/v5/pgxpool"
)

func processingClient(pool *pgxpool.Pool, enabled bool) paymentv1connect.PaymentServiceClient {
	store := &storage.Postgres{Pool: pool}
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{
		Service: payment.NewService(store), Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool}),
		DevProcessingEnabled: enabled,
	})
	return paymentv1connect.NewPaymentServiceClient(connect.NewClient(connectinprocess.New(server)))
}

func newPayment(t *testing.T, client paymentv1connect.PaymentServiceClient) (*paymentv1.CreatePaymentRequest, string) {
	t.Helper()
	req := &paymentv1.CreatePaymentRequest{MerchantId: fmt.Sprintf("phase2-%d", time.Now().UnixNano()), IdempotencyKey: "order", Amount: 740, Currency: "JPY"}
	res, err := client.CreatePayment(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return req, res.GetPayment().GetPaymentId()
}

func simulate(t *testing.T, client paymentv1connect.PaymentServiceClient, id string, scenario paymentv1.SimulationScenario) *paymentv1.Payment {
	t.Helper()
	res, err := client.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{PaymentId: id, Scenario: scenario})
	if err != nil {
		t.Fatal(err)
	}
	return res.GetPayment()
}

func providerFacts(t *testing.T, pool *pgxpool.Pool, id string) (string, int, int) {
	t.Helper()
	var outcome string
	var attempts, charges int
	err := pool.QueryRow(context.Background(), `SELECT outcome, attempts, CASE WHEN logical_charge_id IS NULL THEN 0 ELSE 1 END
        FROM simulated_provider_operations WHERE payment_id = $1`, id).Scan(&outcome, &attempts, &charges)
	if err != nil {
		t.Fatal(err)
	}
	return outcome, attempts, charges
}

func TestApprovedAndDuplicateProcessing(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	createReq, id := newPayment(t, client)
	first := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE)
	if first.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED || first.GetCompletedAt() == nil || first.GetProviderOperationId() == "" {
		t.Fatalf("approved payment = %+v", first)
	}
	repeated := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE)
	if repeated.GetProviderOperationId() != first.GetProviderOperationId() || repeated.GetStatus() != first.GetStatus() {
		t.Fatalf("duplicate processing changed payment: %+v", repeated)
	}
	recreated, err := client.CreatePayment(context.Background(), createReq)
	if err != nil || recreated.GetPayment().GetPaymentId() != id || recreated.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED {
		t.Fatalf("Phase 1 idempotency after processing = %+v, %v", recreated, err)
	}
	got, err := client.GetPayment(context.Background(), &paymentv1.GetPaymentRequest{PaymentId: id})
	if err != nil || got.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestDeclineAndInvalidTransitions(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	_, id := newPayment(t, client)
	if _, err := client.ReconcilePayment(context.Background(), &paymentv1.ReconcilePaymentRequest{PaymentId: id}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("reconcile CREATED error = %v", err)
	}
	// The database trigger rejects invalid transitions even if a future caller
	// bypasses the processing service.
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET status = 'COMPLETED' WHERE id = $1`, id); err == nil {
		t.Fatal("database accepted CREATED -> COMPLETED")
	}
	declined := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_DECLINE)
	if declined.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED || declined.GetFailedAt() == nil {
		t.Fatalf("declined payment = %+v", declined)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET status = 'PROCESSING' WHERE id = $1`, id); err == nil {
		t.Fatal("database accepted FAILED -> PROCESSING")
	}
	if _, err := client.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{PaymentId: id, Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("changed scenario error = %v", err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "DECLINED" || attempts != 1 || charges != 0 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestDatabaseRequiresConfirmedOutcomeAndStableOperation(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	created, err := payment.NewService(store).Create(context.Background(), payment.CreateInput{
		MerchantID: fmt.Sprintf("guard-%d", time.Now().UnixNano()), IdempotencyKey: "key", Amount: 100, Currency: "JPY",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.Claim(context.Background(), created.ID, banking.Approve,
		created.ID, "123e4567-e89b-42d3-a456-426614174021", 30*time.Second)
	if err != nil || !ok || claimed.Status != payment.StatusProcessing {
		t.Fatalf("claim = %+v, %t, %v", claimed, ok, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET status = 'COMPLETED', completed_at = now(),
		processing_token = NULL, processing_lease_until = NULL WHERE id = $1`, created.ID); err == nil {
		t.Fatal("database accepted COMPLETED without provider approval")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET status = 'FAILED', failed_at = now(),
		processing_token = NULL, processing_lease_until = NULL WHERE id = $1`, created.ID); err == nil {
		t.Fatal("database accepted FAILED without provider decline")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET provider_operation_id = $2 WHERE id = $1`, created.ID,
		"123e4567-e89b-42d3-a456-426614174022"); err == nil {
		t.Fatal("database accepted a changed provider operation ID")
	}
}

func TestTemporaryFailureReconcilesAfterRestart(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	_, id := newPayment(t, client)
	uncertain := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_TEMPORARY_FAILURE)
	if uncertain.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED || uncertain.GetReconciliationRequiredAt() == nil {
		t.Fatalf("temporary failure = %+v", uncertain)
	}
	_ = simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_TEMPORARY_FAILURE)
	if _, attempts, charges := providerFacts(t, pool, id); attempts != 1 || charges != 0 {
		t.Fatalf("duplicate retried provider: attempts=%d, charges=%d", attempts, charges)
	}
	// A new service instance shares only durable PostgreSQL state with the first.
	restarted := processingClient(pool, true)
	res, err := restarted.ReconcilePayment(context.Background(), &paymentv1.ReconcilePaymentRequest{PaymentId: id})
	if err != nil || res.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED {
		t.Fatalf("reconcile = %+v, %v", res, err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "APPROVED" || attempts != 2 || charges != 1 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestTimeoutStaysUncertain(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	_, id := newPayment(t, client)
	first := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_NETWORK_TIMEOUT)
	if first.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED || first.GetFailedAt() != nil {
		t.Fatalf("timeout = %+v", first)
	}
	res, err := client.ReconcilePayment(context.Background(), &paymentv1.ReconcilePaymentRequest{PaymentId: id})
	if err != nil || res.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED {
		t.Fatalf("reconcile timeout = %+v, %v", res, err)
	}
	if outcome, _, charges := providerFacts(t, pool, id); outcome != "PENDING" || charges != 0 {
		t.Fatalf("provider = %s, charges=%d", outcome, charges)
	}
}

func TestLostApprovalResponseReconcilesAfterRestart(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	_, id := newPayment(t, client)
	first := simulate(t, client, id, paymentv1.SimulationScenario_SIMULATION_SCENARIO_LOST_APPROVAL_RESPONSE)
	if first.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED {
		t.Fatalf("lost response = %+v", first)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
	restarted := processingClient(pool, true)
	res, err := restarted.ReconcilePayment(context.Background(), &paymentv1.ReconcilePaymentRequest{PaymentId: id})
	if err != nil || res.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED {
		t.Fatalf("reconcile = %+v, %v", res, err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("duplicate charge: %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestConcurrentProcessingOneLogicalCharge(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, true)
	_, id := newPayment(t, client)
	const workers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{PaymentId: id, Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE})
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got, err := client.GetPayment(context.Background(), &paymentv1.GetPaymentRequest{PaymentId: id})
	if err != nil || got.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, id); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

type failFinishOnce struct {
	*storage.Postgres
	mu     sync.Mutex
	failed bool
}

func (s *failFinishOnce) Finish(ctx context.Context, id, token, target, reason string) (payment.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed {
		s.failed = true
		return payment.Payment{}, errors.New("injected database update failure")
	}
	return s.Postgres.Finish(ctx, id, token, target, reason)
}

func TestRecoveryAfterPaymentUpdateFailure(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	created, err := payment.NewService(store).Create(context.Background(), payment.CreateInput{
		MerchantID: fmt.Sprintf("db-failure-%d", time.Now().UnixNano()), IdempotencyKey: "key", Amount: 900, Currency: "JPY",
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := payment.NewProcessor(&failFinishOnce{Postgres: store}, &banking.Postgres{Pool: pool})
	if _, err := processor.Process(context.Background(), created.ID, banking.Approve); err == nil {
		t.Fatal("expected injected update failure")
	}
	before, err := store.Get(context.Background(), created.ID)
	if err != nil || before.Status != payment.StatusProcessing {
		t.Fatalf("before recovery = %+v, %v", before, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET processing_lease_until = now() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	restarted := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	after, err := restarted.Reconcile(context.Background(), created.ID)
	if err != nil || after.Status != payment.StatusCompleted || after.ProviderOperationID != before.ProviderOperationID {
		t.Fatalf("after recovery = %+v, %v", after, err)
	}
	if outcome, attempts, charges := providerFacts(t, pool, created.ID); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestSimulationDisabledByDefault(t *testing.T) {
	pool := testPool(t)
	client := processingClient(pool, false)
	_, id := newPayment(t, client)
	_, err := client.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{PaymentId: id, Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("disabled simulation error = %v", err)
	}
}

func TestHTTPDevSimulation(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{
		Service: payment.NewService(store), Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool}),
		DevProcessingEnabled: true,
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	_, id := newPayment(t, processingClient(pool, true))
	body := fmt.Sprintf(`{"paymentId":"%s","scenario":"SIMULATION_SCENARIO_APPROVE"}`, id)
	resp, err := http.Post(httpServer.URL+"/payflow.v1.PaymentService/SimulatePayment", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
	var result struct {
		Payment struct {
			Status string `json:"status"`
		} `json:"payment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Payment.Status != "PAYMENT_STATUS_COMPLETED" {
		t.Fatalf("HTTP status mapping = %+v", result)
	}
}
