package payment

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"
	paymentv1 "payflow/gen/go/payflow/v1"
	"payflow/internal/banking"
)

type fakeOrchestrator struct {
	payment Payment
	err     error
	calls   int
}

func (f *fakeOrchestrator) Request(context.Context, string, banking.Scenario) (Payment, error) {
	f.calls++
	return f.payment, f.err
}

func TestSimulatePaymentQueuesWorkflowAndMapsCurrentStatus(t *testing.T) {
	fake := &fakeOrchestrator{payment: Payment{ID: "123e4567-e89b-42d3-a456-426614174000", Status: StatusCreated}}
	h := &Handler{Orchestrator: fake, DevProcessingEnabled: true}
	resp, err := h.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{
		PaymentId: fake.payment.ID, Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE,
	})
	if err != nil || resp.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED || fake.calls != 1 {
		t.Fatalf("queued payment = %+v, calls=%d, err=%v", resp, fake.calls, err)
	}
}

func TestSimulatePaymentReportsTemporalOutage(t *testing.T) {
	h := &Handler{Orchestrator: &fakeOrchestrator{err: ErrWorkflowUnavailable}, DevProcessingEnabled: true}
	_, err := h.SimulatePayment(context.Background(), &paymentv1.SimulatePaymentRequest{
		PaymentId: "123e4567-e89b-42d3-a456-426614174000", Scenario: paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE,
	})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("Temporal outage code = %v", err)
	}
}
