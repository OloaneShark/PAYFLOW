package orchestration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"payflow/internal/banking"
	"payflow/internal/payment"
)

func workflowInput() WorkflowInput {
	return WorkflowInput{PaymentID: "123e4567-e89b-42d3-a456-426614174000", Scenario: banking.Approve,
		ReconcileInterval: time.Second, ReconcileMaxInterval: 4 * time.Second, ReconcileWindow: 12 * time.Second}
}

func TestWorkflowReconcilesUncertainOutcome(t *testing.T) {
	env := new(testsuite.WorkflowTestSuite).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(context.Context, string, banking.Scenario) (string, error) { return "", nil }, activity.RegisterOptions{Name: AdvanceActivityName})
	env.OnActivity(AdvanceActivityName, mock.Anything, workflowInput().PaymentID, banking.Approve).
		Return(payment.StatusReconciliationRequired, nil).Once()
	env.OnActivity(AdvanceActivityName, mock.Anything, workflowInput().PaymentID, banking.Approve).
		Return(payment.StatusCompleted, nil).Once()
	env.ExecuteWorkflow(PaymentWorkflow, workflowInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.AssertNumberOfCalls(t, AdvanceActivityName, 2)
}

func TestWorkflowLeavesPermanentlyUnknownUnresolved(t *testing.T) {
	env := new(testsuite.WorkflowTestSuite).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(context.Context, string, banking.Scenario) (string, error) { return "", nil }, activity.RegisterOptions{Name: AdvanceActivityName})
	var calls int
	env.OnActivity(AdvanceActivityName, mock.Anything, workflowInput().PaymentID, banking.Approve).
		Return(payment.StatusReconciliationRequired, nil).Run(func(mock.Arguments) { calls++ })
	env.ExecuteWorkflow(PaymentWorkflow, workflowInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not reach reconciliation deadline")
	}
	if calls < 2 {
		t.Fatalf("advance activity calls = %d", calls)
	}
}
