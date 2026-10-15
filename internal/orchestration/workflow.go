package orchestration

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"payflow/internal/banking"
	"payflow/internal/payment"
)

const AdvanceActivityName = "AdvancePayment"

type WorkflowInput struct {
	PaymentID            string
	Scenario             banking.Scenario
	ReconcileInterval    time.Duration
	ReconcileMaxInterval time.Duration
	ReconcileWindow      time.Duration
}

func WorkflowID(paymentID string) string { return "payflow-payment-" + paymentID }

// PaymentWorkflow contains only deterministic control flow. The database,
// provider, clocks outside workflow.Now, and random IDs stay in activities.
func PaymentWorkflow(ctx workflow.Context, in WorkflowInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2,
			MaximumInterval: 10 * time.Second, MaximumAttempts: 3,
		},
	})
	deadline := workflow.Now(ctx).Add(in.ReconcileWindow)
	delay := in.ReconcileInterval
	finalAttempt := false
	for {
		var status string
		err := workflow.ExecuteActivity(ctx, AdvanceActivityName, in.PaymentID, in.Scenario).Get(ctx, &status)
		if err == nil && (status == payment.StatusCompleted || status == payment.StatusFailed) {
			return nil
		}
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			// A worker restart can delay delivery of an activity result past the
			// deadline. Check the provider once more before closing the workflow.
			if !finalAttempt {
				finalAttempt = true
				continue
			}
			return nil
		}
		if delay > remaining {
			delay = remaining
		}
		if err := workflow.Sleep(ctx, delay); err != nil {
			return err
		}
		delay *= 2
		if delay > in.ReconcileMaxInterval {
			delay = in.ReconcileMaxInterval
		}
	}
}
