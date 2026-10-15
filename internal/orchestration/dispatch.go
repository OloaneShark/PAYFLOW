package orchestration

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"payflow/internal/banking"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

var ErrTemporalUnavailable = payment.ErrWorkflowUnavailable

type DispatchStore interface {
	Enqueue(context.Context, string, banking.Scenario, string) (storage.DispatchRequest, error)
	PendingDispatches(context.Context, int) ([]storage.DispatchRequest, error)
	MarkDispatched(context.Context, string) error
	RecordDispatchError(context.Context, string, error) error
	Get(context.Context, string) (payment.Payment, error)
}

type Starter interface {
	Start(context.Context, storage.DispatchRequest) error
}

type Coordinator struct {
	Store   DispatchStore
	Starter Starter
}

// Request commits the dispatch row before contacting Temporal. A failed start
// is visible to the caller and remains available to the worker's scanner.
func (c *Coordinator) Request(ctx context.Context, id string, scenario banking.Scenario) (payment.Payment, error) {
	if !scenario.Valid() || !payment.ValidPaymentID(id) {
		return payment.Payment{}, payment.ErrInvalidArgument
	}
	p, err := c.Store.Get(ctx, id)
	if err != nil {
		return payment.Payment{}, err
	}
	if p.ProcessingScenario != "" && p.ProcessingScenario != scenario {
		return payment.Payment{}, payment.ErrScenarioConflict
	}
	if p.Status == payment.StatusCompleted || p.Status == payment.StatusFailed {
		return p, nil
	}
	req, err := c.Store.Enqueue(ctx, id, scenario, WorkflowID(id))
	if err != nil {
		return payment.Payment{}, err
	}
	if req.DispatchedAt == nil {
		if err := c.dispatchOne(ctx, req); err != nil {
			return payment.Payment{}, err
		}
	}
	return c.Store.Get(ctx, id)
}

func (c *Coordinator) DispatchPending(ctx context.Context) (int, error) {
	requests, err := c.Store.PendingDispatches(ctx, 100)
	if err != nil {
		return 0, err
	}
	var dispatchErrors []error
	for _, req := range requests {
		if err := c.dispatchOne(ctx, req); err != nil {
			dispatchErrors = append(dispatchErrors, err)
		}
	}
	return len(requests), errors.Join(dispatchErrors...)
}

func (c *Coordinator) dispatchOne(ctx context.Context, req storage.DispatchRequest) error {
	if err := c.Starter.Start(ctx, req); err != nil {
		_ = c.Store.RecordDispatchError(ctx, req.PaymentID, err)
		return fmt.Errorf("%w: %v", ErrTemporalUnavailable, err)
	}
	return c.Store.MarkDispatched(ctx, req.PaymentID)
}

type TemporalStarter struct{ Config Config }

func (s *TemporalStarter) Start(ctx context.Context, req storage.DispatchRequest) error {
	clientCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := client.DialContext(clientCtx, client.Options{HostPort: s.Config.Address, Namespace: s.Config.Namespace})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.ExecuteWorkflow(clientCtx, client.StartWorkflowOptions{
		ID: req.WorkflowID, TaskQueue: s.Config.TaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
	}, PaymentWorkflow, WorkflowInput{
		PaymentID: req.PaymentID, Scenario: req.Scenario,
		ReconcileInterval:    s.Config.ReconcileInterval,
		ReconcileMaxInterval: s.Config.ReconcileMaxInterval,
		ReconcileWindow:      s.Config.ReconcileWindow,
	})
	if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
		return nil
	}
	return err
}
