package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"payflow/internal/banking"
)

type ProcessingStore interface {
	Get(context.Context, string) (Payment, error)
	Claim(context.Context, string, banking.Scenario, string, string, time.Duration) (Payment, bool, error)
	ClaimReconciliation(context.Context, string, string, time.Duration) (Payment, bool, error)
	Finish(context.Context, string, string, string, string) (Payment, error)
}

type Processor struct {
	store    ProcessingStore
	provider banking.Provider
	lease    time.Duration
}

func NewProcessor(store ProcessingStore, provider banking.Provider) *Processor {
	return &Processor{store: store, provider: provider, lease: 30 * time.Second}
}

func (p *Processor) Process(ctx context.Context, id string, scenario banking.Scenario) (Payment, error) {
	if !validUUID(id) || !scenario.Valid() {
		return Payment{}, ErrInvalidArgument
	}
	opID, err := newUUID()
	if err != nil {
		return Payment{}, err
	}
	token, err := newUUID()
	if err != nil {
		return Payment{}, err
	}
	claimed, ok, err := p.store.Claim(ctx, id, scenario, opID, token, p.lease)
	if err != nil || !ok {
		return claimed, err
	}
	return p.execute(ctx, claimed, token)
}

func (p *Processor) Reconcile(ctx context.Context, id string) (Payment, error) {
	if !validUUID(id) {
		return Payment{}, ErrInvalidArgument
	}
	token, err := newUUID()
	if err != nil {
		return Payment{}, err
	}
	claimed, ok, err := p.store.ClaimReconciliation(ctx, id, token, p.lease)
	if err != nil || !ok {
		return claimed, err
	}
	op := banking.Operation{ID: claimed.ProviderOperationID, PaymentID: claimed.ID, Scenario: claimed.ProcessingScenario}
	outcome, err := p.provider.Lookup(ctx, op)
	if err != nil {
		return p.uncertain(ctx, claimed, token, err)
	}
	if outcome == banking.Approved || outcome == banking.Declined {
		return p.finishOutcome(ctx, claimed, token, outcome)
	}
	return p.execute(ctx, claimed, token)
}

func (p *Processor) execute(ctx context.Context, claimed Payment, token string) (Payment, error) {
	op := banking.Operation{ID: claimed.ProviderOperationID, PaymentID: claimed.ID, Scenario: claimed.ProcessingScenario}
	outcome, err := p.provider.Execute(ctx, op)
	if err != nil {
		return p.uncertain(ctx, claimed, token, err)
	}
	return p.finishOutcome(ctx, claimed, token, outcome)
}

func (p *Processor) finishOutcome(ctx context.Context, claimed Payment, token string, outcome banking.Outcome) (Payment, error) {
	var target string
	switch outcome {
	case banking.Approved:
		target = StatusCompleted
	case banking.Declined:
		target = StatusFailed
	default:
		return p.uncertain(ctx, claimed, token, errors.New("provider outcome is not confirmed"))
	}
	if !ValidTransition(claimed.Status, target) {
		return Payment{}, ErrInvalidTransition
	}
	return p.store.Finish(ctx, claimed.ID, token, target, "")
}

func (p *Processor) uncertain(ctx context.Context, claimed Payment, token string, cause error) (Payment, error) {
	if !ValidTransition(claimed.Status, StatusReconciliationRequired) {
		return Payment{}, ErrInvalidTransition
	}
	result, err := p.store.Finish(ctx, claimed.ID, token, StatusReconciliationRequired, cause.Error())
	if err != nil {
		return Payment{}, fmt.Errorf("record uncertain provider outcome: %w", err)
	}
	return result, nil
}
