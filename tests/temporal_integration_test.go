package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
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

// These tests need both TEST_DATABASE_URL and TEST_TEMPORAL_ADDRESS. Each run
// uses unique payment IDs and a unique task queue in the local Temporal server.
func temporalSetup(t *testing.T) (*pgxpool.Pool, *storage.Postgres, orchestration.Config, client.Client) {
	t.Helper()
	address := os.Getenv("TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("TEST_TEMPORAL_ADDRESS is not set")
	}
	pool := testPool(t)
	config := orchestration.Config{Address: address, Namespace: "default",
		TaskQueue:         fmt.Sprintf("payflow-test-%d", time.Now().UnixNano()),
		ReconcileInterval: 200 * time.Millisecond, ReconcileMaxInterval: 800 * time.Millisecond,
		ReconcileWindow: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	temporalClient, err := client.DialContext(ctx, client.Options{HostPort: config.Address, Namespace: config.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(temporalClient.Close)
	return pool, &storage.Postgres{Pool: pool}, config, temporalClient
}

func startTemporalTestWorker(t *testing.T, c client.Client, config orchestration.Config, store *storage.Postgres, pool *pgxpool.Pool) worker.Worker {
	t.Helper()
	w := worker.New(c, config.TaskQueue, worker.Options{WorkerStopTimeout: 3 * time.Second, StickyScheduleToStartTimeout: time.Second})
	w.RegisterWorkflow(orchestration.PaymentWorkflow)
	a := &orchestration.Activities{Store: store, Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool})}
	w.RegisterActivityWithOptions(a.Advance, activity.RegisterOptions{Name: orchestration.AdvanceActivityName})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	return w
}

func createTemporalPayment(t *testing.T, store *storage.Postgres) payment.Payment {
	t.Helper()
	p, err := payment.NewService(store).Create(context.Background(), payment.CreateInput{
		MerchantID:     fmt.Sprintf("temporal-%d", time.Now().UnixNano()),
		IdempotencyKey: "one", Amount: 900, Currency: "JPY",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitPaymentStatus(t *testing.T, store *storage.Postgres, id, want string) payment.Payment {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		p, err := store.Get(context.Background(), id)
		if err == nil && p.Status == want {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	p, err := store.Get(context.Background(), id)
	t.Fatalf("payment %s status = %s, want %s (read error: %v)", id, p.Status, want, err)
	return payment.Payment{}
}

func TestTemporalProcessingOutcomesAndIdempotency(t *testing.T) {
	pool, store, config, temporalClient := temporalSetup(t)
	w := startTemporalTestWorker(t, temporalClient, config, store, pool)
	defer w.Stop()
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	for _, tc := range []struct {
		name     string
		scenario banking.Scenario
		final    string
		charges  int
	}{
		{"approve", banking.Approve, payment.StatusCompleted, 1},
		{"decline", banking.Decline, payment.StatusFailed, 0},
		{"temporary", banking.TemporaryFailure, payment.StatusCompleted, 1},
		{"lost-response", banking.LostApprovalResponse, payment.StatusCompleted, 1},
		{"timeout", banking.NetworkTimeout, payment.StatusReconciliationRequired, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := createTemporalPayment(t, store)
			const callers = 8
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := coordinator.Request(context.Background(), created.ID, tc.scenario)
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			p := waitPaymentStatus(t, store, created.ID, tc.final)
			if p.ProviderOperationID == "" {
				t.Fatal("missing stable provider operation ID")
			}
			if _, err := coordinator.Request(context.Background(), created.ID, tc.scenario); err != nil {
				t.Fatal(err)
			}
			other := banking.Approve
			if tc.scenario == banking.Approve {
				other = banking.Decline
			}
			if _, err := coordinator.Request(context.Background(), created.ID, other); !errors.Is(err, payment.ErrScenarioConflict) {
				t.Fatalf("changed scenario error = %v", err)
			}
			_, _, charges := providerFacts(t, pool, created.ID)
			if charges != tc.charges {
				t.Fatalf("logical charges = %d, want %d", charges, tc.charges)
			}
			var providerRows int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM simulated_provider_operations WHERE payment_id=$1`, created.ID).Scan(&providerRows); err != nil || providerRows != 1 {
				t.Fatalf("provider rows = %d, err = %v", providerRows, err)
			}
			if tc.scenario == banking.NetworkTimeout {
				waitCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				if err := temporalClient.GetWorkflow(waitCtx, orchestration.WorkflowID(created.ID), "").Get(waitCtx, nil); err != nil {
					t.Fatalf("uncertain workflow deadline: %v", err)
				}
				if final, err := store.Get(context.Background(), created.ID); err != nil || final.Status != payment.StatusReconciliationRequired {
					t.Fatalf("status after reconciliation window = %s, err = %v", final.Status, err)
				}
			}
			var dispatchRows int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM payment_workflow_dispatch WHERE payment_id=$1`, created.ID).Scan(&dispatchRows); err != nil || dispatchRows != 1 {
				t.Fatalf("dispatch rows = %d, err = %v", dispatchRows, err)
			}
		})
	}
}

type failGetOnce struct {
	*storage.Postgres
	failed atomic.Bool
}

func (s *failGetOnce) Get(ctx context.Context, id string) (payment.Payment, error) {
	if s.failed.CompareAndSwap(false, true) {
		return payment.Payment{}, errors.New("injected temporary database read failure")
	}
	return s.Postgres.Get(ctx, id)
}

func TestTemporalActivityRetriesTemporaryDatabaseFailure(t *testing.T) {
	pool, store, config, temporalClient := temporalSetup(t)
	flaky := &failGetOnce{Postgres: store}
	w := worker.New(temporalClient, config.TaskQueue, worker.Options{WorkerStopTimeout: 3 * time.Second, StickyScheduleToStartTimeout: time.Second})
	w.RegisterWorkflow(orchestration.PaymentWorkflow)
	a := &orchestration.Activities{Store: flaky, Processor: payment.NewProcessor(flaky, &banking.Postgres{Pool: pool})}
	w.RegisterActivityWithOptions(a.Advance, activity.RegisterOptions{Name: orchestration.AdvanceActivityName})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	created := createTemporalPayment(t, store)
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	if _, err := coordinator.Request(context.Background(), created.ID, banking.Approve); err != nil {
		t.Fatal(err)
	}
	completed := waitPaymentStatus(t, store, created.ID, payment.StatusCompleted)
	if !flaky.failed.Load() {
		t.Fatal("database failure was not exercised")
	}
	if outcome, attempts, charges := providerFacts(t, pool, created.ID); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider after activity retry = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
	if status, err := a.Advance(context.Background(), created.ID, banking.Approve); err != nil || status != payment.StatusCompleted {
		t.Fatalf("repeated activity = %s, %v", status, err)
	}
	if repeated, err := store.Get(context.Background(), created.ID); err != nil || repeated.ProviderOperationID != completed.ProviderOperationID {
		t.Fatalf("operation changed after repeated activity: %+v, %v", repeated, err)
	}
	if _, attempts, charges := providerFacts(t, pool, created.ID); attempts != 1 || charges != 1 {
		t.Fatalf("repeated activity changed provider: attempts=%d, charges=%d", attempts, charges)
	}
}

func TestTemporalRecoversAfterPaymentUpdateFailure(t *testing.T) {
	pool, store, config, temporalClient := temporalSetup(t)
	w := worker.New(temporalClient, config.TaskQueue, worker.Options{WorkerStopTimeout: 3 * time.Second, StickyScheduleToStartTimeout: time.Second})
	w.RegisterWorkflow(orchestration.PaymentWorkflow)
	flaky := &failFinishOnce{Postgres: store}
	a := &orchestration.Activities{Store: store, Processor: payment.NewProcessor(flaky, &banking.Postgres{Pool: pool})}
	w.RegisterActivityWithOptions(a.Advance, activity.RegisterOptions{Name: orchestration.AdvanceActivityName})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	created := createTemporalPayment(t, store)
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	if _, err := coordinator.Request(context.Background(), created.ID, banking.Approve); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		flaky.mu.Lock()
		failed := flaky.failed
		flaky.mu.Unlock()
		if failed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	flaky.mu.Lock()
	failed := flaky.failed
	flaky.mu.Unlock()
	if !failed {
		t.Fatal("payment update failure was not exercised")
	}
	before, err := store.Get(context.Background(), created.ID)
	if err != nil || before.Status != payment.StatusProcessing {
		t.Fatalf("before recovery = %+v, %v", before, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET processing_lease_until = now() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	completed := waitPaymentStatus(t, store, created.ID, payment.StatusCompleted)
	if completed.ProviderOperationID != before.ProviderOperationID {
		t.Fatal("provider operation ID changed")
	}
	if outcome, attempts, charges := providerFacts(t, pool, created.ID); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider after payment update recovery = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}

func TestTemporalDispatchRecoveryAndWorkerRestart(t *testing.T) {
	pool, store, config, temporalClient := temporalSetup(t)
	created := createTemporalPayment(t, store)
	offline := config
	offline.Address = "127.0.0.1:1"
	failed := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: offline}}
	if _, err := failed.Request(context.Background(), created.ID, banking.LostApprovalResponse); !errors.Is(err, payment.ErrWorkflowUnavailable) {
		t.Fatalf("unavailable start error = %v", err)
	}
	req, err := store.Dispatch(context.Background(), created.ID)
	if err != nil || req.DispatchedAt != nil {
		t.Fatalf("durable pending dispatch = %+v, %v", req, err)
	}
	var providerRows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM simulated_provider_operations WHERE payment_id=$1`, created.ID).Scan(&providerRows); err != nil || providerRows != 0 {
		t.Fatalf("provider rows before worker = %d, err=%v", providerRows, err)
	}
	coordinator := &orchestration.Coordinator{Store: store, Starter: &orchestration.TemporalStarter{Config: config}}
	if n, err := coordinator.DispatchPending(context.Background()); err != nil || n < 1 {
		t.Fatalf("dispatch recovery = %d, %v", n, err)
	}
	// Temporal accepts the workflow while no worker is running. A later worker
	// must resume it from server history, not create a new provider operation.
	w := startTemporalTestWorker(t, temporalClient, config, store, pool)
	waitPaymentStatus(t, store, created.ID, payment.StatusReconciliationRequired)
	w.Stop()
	w = startTemporalTestWorker(t, temporalClient, config, store, pool)
	defer w.Stop()
	completed := waitPaymentStatus(t, store, created.ID, payment.StatusCompleted)
	if completed.ProviderOperationID == "" {
		t.Fatal("missing operation ID after restart")
	}
	if outcome, attempts, charges := providerFacts(t, pool, created.ID); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider after restart = %s, attempts=%d, charges=%d", outcome, attempts, charges)
	}
}
