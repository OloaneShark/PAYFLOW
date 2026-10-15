package orchestration

import (
	"context"
	"go.temporal.io/sdk/activity"
	"log/slog"

	"payflow/internal/banking"
	"payflow/internal/payment"
)

type Activities struct {
	Store     payment.ProcessingStore
	Processor *payment.Processor
}

// Advance is safe to run again after a worker crash or an activity timeout.
// The Phase 2 claim and provider operation ID decide what can be repeated.
func (a *Activities) Advance(ctx context.Context, id string, scenario banking.Scenario) (string, error) {
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		slog.InfoContext(ctx, "payment activity started", "payment_id", id, "workflow_id", info.WorkflowExecution.ID, "activity_name", AdvanceActivityName)
	}
	p, err := a.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if p.ProcessingScenario != "" && p.ProcessingScenario != scenario {
		return "", payment.ErrScenarioConflict
	}
	switch p.Status {
	case payment.StatusCreated:
		p, err = a.Processor.Process(ctx, id, scenario)
	case payment.StatusProcessing, payment.StatusReconciliationRequired:
		p, err = a.Processor.Reconcile(ctx, id)
	case payment.StatusCompleted, payment.StatusFailed:
		return p.Status, nil
	default:
		return "", payment.ErrInvalidTransition
	}
	if err != nil {
		return "", err
	}
	return p.Status, nil
}
