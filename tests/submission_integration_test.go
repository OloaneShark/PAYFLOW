package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/banking"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

type submissionStarter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *submissionStarter) Start(_ context.Context, _ storage.DispatchRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

type oneDispatchStore struct {
	*storage.Postgres
	id string
}

func (s *oneDispatchStore) PendingDispatches(ctx context.Context, _ int) ([]storage.DispatchRequest, error) {
	req, err := s.Dispatch(ctx, s.id)
	if err != nil {
		return nil, err
	}
	if req.DispatchedAt != nil {
		return nil, nil
	}
	return []storage.DispatchRequest{req}, nil
}

func TestSubmissionDurabilityConcurrentAndDispatchRetry(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := &storage.Postgres{Pool: pool}
	created, err := payment.NewService(store).Create(ctx, payment.CreateInput{MerchantID: fmt.Sprintf("submit-%d", time.Now().UnixNano()), IdempotencyKey: "one", Amount: 700, Currency: "JPY"})
	if err != nil {
		t.Fatal(err)
	}
	submitter := &orchestration.SubmissionService{Store: store}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := submitter.Submit(ctx, created.ID); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_workflow_dispatch WHERE payment_id=$1`, created.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("dispatch rows = %d, %v", rows, err)
	}
	req, err := store.Dispatch(ctx, created.ID)
	if err != nil || req.WorkflowID != orchestration.WorkflowID(created.ID) || req.Scenario != orchestration.OutcomePolicy(created.ID) {
		t.Fatalf("dispatch = %+v, %v", req, err)
	}
	opposite := banking.Approve
	if req.Scenario == banking.Approve {
		opposite = banking.Decline
	}
	if _, err := store.Enqueue(ctx, created.ID, opposite, req.WorkflowID); !errors.Is(err, payment.ErrScenarioConflict) {
		t.Fatalf("conflicting scenario: %v", err)
	}
	var providerRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM simulated_provider_operations WHERE payment_id=$1`, created.ID).Scan(&providerRows); err != nil || providerRows != 0 {
		t.Fatalf("HTTP submission processed payment: rows=%d, err=%v", providerRows, err)
	}
	starter := &submissionStarter{err: errors.New("Temporal unavailable")}
	coordinator := &orchestration.Coordinator{Store: &oneDispatchStore{Postgres: store, id: created.ID}, Starter: starter}
	if _, err := coordinator.DispatchPending(ctx); err == nil {
		t.Fatal("Temporal outage was hidden")
	}
	req, err = store.Dispatch(ctx, created.ID)
	if err != nil || req.DispatchedAt != nil {
		t.Fatalf("outage lost durable queue: %+v %v", req, err)
	}
	starter.err = nil
	if _, err := coordinator.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if starter.calls != 2 {
		t.Fatalf("repeat dispatched workflow %d times", starter.calls)
	}
}

func TestSubmissionAfterDatabaseConnectionInterruption(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	created, err := payment.NewService(&storage.Postgres{Pool: pool}).Create(ctx, payment.CreateInput{MerchantID: fmt.Sprintf("outage-%d", time.Now().UnixNano()), IdempotencyKey: "one", Amount: 300, Currency: "JPY"})
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := (&orchestration.SubmissionService{Store: &storage.Postgres{Pool: pool}}).Submit(ctx, created.ID); err == nil {
		t.Fatal("submission succeeded with a closed database pool")
	}
	reopened, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := (&orchestration.SubmissionService{Store: &storage.Postgres{Pool: reopened}}).Submit(ctx, created.ID); err != nil {
		t.Fatalf("submission did not recover after reconnection: %v", err)
	}
	var count int
	if err := reopened.QueryRow(ctx, `SELECT count(*) FROM payment_workflow_dispatch WHERE payment_id=$1`, created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovered queue = %d, %v", count, err)
	}
}
