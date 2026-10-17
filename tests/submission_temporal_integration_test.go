package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"payflow/internal/banking"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func paymentForPolicy(t *testing.T, store *storage.Postgres, want banking.Scenario) payment.Payment {
	t.Helper()
	for i := 0; i < 100; i++ {
		p, err := payment.NewService(store).Create(context.Background(), payment.CreateInput{
			MerchantID:     fmt.Sprintf("submit-temporal-%d-%d", time.Now().UnixNano(), i),
			IdempotencyKey: "one", Amount: 900, Currency: "JPY",
		})
		if err != nil {
			t.Fatal(err)
		}
		if orchestration.OutcomePolicy(p.ID) == want {
			return p
		}
	}
	t.Fatal("could not create payment for simulation policy")
	return payment.Payment{}
}

func TestSubmittedPaymentsUseOneWorkflowAndOneAccountingOutcome(t *testing.T) {
	pool, store, config, temporalClient := temporalSetup(t)
	ctx := context.Background()
	submitter := &orchestration.SubmissionService{Store: store}
	for _, tc := range []struct {
		scenario banking.Scenario
		status   string
		charges  int
	}{
		{banking.Approve, payment.StatusCompleted, 1},
		{banking.Decline, payment.StatusFailed, 0},
	} {
		created := paymentForPolicy(t, store, tc.scenario)
		if queued, err := submitter.Submit(ctx, created.ID); err != nil || queued.Status != payment.StatusCreated {
			t.Fatalf("submission: %+v %v", queued, err)
		}
		coordinator := &orchestration.Coordinator{Store: &oneDispatchStore{Postgres: store, id: created.ID}, Starter: &orchestration.TemporalStarter{Config: config}}
		if _, err := coordinator.DispatchPending(ctx); err != nil {
			t.Fatal(err)
		}
		// Temporal accepted the stable workflow while no worker was running.
		// Starting a worker later must consume the durable history.
		w := startTemporalTestWorker(t, temporalClient, config, store, pool)
		final := waitPaymentStatus(t, store, created.ID, tc.status)
		w.Stop()
		if final.ProviderOperationID == "" {
			t.Fatal("missing stable provider operation ID")
		}
		if repeated, err := submitter.Submit(ctx, created.ID); err != nil || repeated.ProviderOperationID != final.ProviderOperationID {
			t.Fatalf("terminal resubmission: %+v %v", repeated, err)
		}
		if _, err := coordinator.DispatchPending(ctx); err != nil {
			t.Fatal(err)
		}
		if _, attempts, charges := providerFacts(t, pool, created.ID); attempts != 1 || charges != tc.charges {
			t.Fatalf("provider attempts=%d charges=%d, want one attempt and %d charges", attempts, charges, tc.charges)
		}
		if journals := journalCount(t, pool, created.ID); journals != tc.charges {
			t.Fatalf("approval journals=%d, want %d", journals, tc.charges)
		}
	}
}
