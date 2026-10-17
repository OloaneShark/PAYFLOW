package orchestration

import (
	"context"
	"crypto/sha256"

	"payflow/internal/banking"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

// SubmissionStore exposes the durable queue without requiring Temporal access
// from the merchant-facing API.
type SubmissionStore interface {
	Get(context.Context, string) (payment.Payment, error)
	Enqueue(context.Context, string, banking.Scenario, string) (storage.DispatchRequest, error)
}

type SubmissionService struct{ Store SubmissionStore }

// OutcomePolicy is stable across restarts and uses a server-generated UUID.
// Its 80/20 approve/decline split is only a mock demonstration policy.
func OutcomePolicy(paymentID string) banking.Scenario {
	digest := sha256.Sum256([]byte(paymentID))
	if digest[0]%5 == 0 {
		return banking.Decline
	}
	return banking.Approve
}

func (s *SubmissionService) Submit(ctx context.Context, id string) (payment.Payment, error) {
	if !payment.ValidPaymentID(id) {
		return payment.Payment{}, payment.ErrInvalidArgument
	}
	p, err := s.Store.Get(ctx, id)
	if err != nil {
		return payment.Payment{}, err
	}
	if p.Status == payment.StatusCompleted || p.Status == payment.StatusFailed {
		return p, nil
	}
	if _, err := s.Store.Enqueue(ctx, id, OutcomePolicy(id), WorkflowID(id)); err != nil {
		return payment.Payment{}, err
	}
	return s.Store.Get(ctx, id)
}
