package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"payflow/internal/banking"
	"payflow/internal/payment"
)

type DispatchRequest struct {
	PaymentID    string
	Scenario     banking.Scenario
	WorkflowID   string
	DispatchedAt *time.Time
}

func (s *Postgres) Enqueue(ctx context.Context, id string, scenario banking.Scenario, workflowID string) (DispatchRequest, error) {
	const insert = `INSERT INTO payment_workflow_dispatch (payment_id, scenario, workflow_id)
        SELECT id, $2, $3 FROM payments
        WHERE id = $1 AND (processing_scenario IS NULL OR processing_scenario = $2)
        ON CONFLICT DO NOTHING`
	if _, err := s.Pool.Exec(ctx, insert, id, scenario, workflowID); err != nil {
		return DispatchRequest{}, fmt.Errorf("enqueue workflow: %w", err)
	}
	req, err := s.Dispatch(ctx, id)
	if err == nil {
		if req.Scenario != scenario || req.WorkflowID != workflowID {
			return DispatchRequest{}, payment.ErrScenarioConflict
		}
		return req, nil
	}
	if !errors.Is(err, payment.ErrNotFound) {
		return DispatchRequest{}, err
	}
	p, err := s.Get(ctx, id)
	if err != nil {
		return DispatchRequest{}, err
	}
	if p.ProcessingScenario != "" && p.ProcessingScenario != scenario {
		return DispatchRequest{}, payment.ErrScenarioConflict
	}
	return DispatchRequest{}, errors.New("workflow request was not enqueued")
}

func (s *Postgres) Dispatch(ctx context.Context, id string) (DispatchRequest, error) {
	const query = `SELECT payment_id, scenario, workflow_id, dispatched_at
        FROM payment_workflow_dispatch WHERE payment_id = $1`
	var req DispatchRequest
	var dispatchedAt *time.Time
	err := s.Pool.QueryRow(ctx, query, id).Scan(&req.PaymentID, &req.Scenario, &req.WorkflowID, &dispatchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DispatchRequest{}, payment.ErrNotFound
	}
	if err != nil {
		return DispatchRequest{}, fmt.Errorf("get workflow dispatch: %w", err)
	}
	req.DispatchedAt = dispatchedAt
	return req, nil
}

func (s *Postgres) PendingDispatches(ctx context.Context, limit int) ([]DispatchRequest, error) {
	const query = `SELECT payment_id, scenario, workflow_id FROM payment_workflow_dispatch
        WHERE dispatched_at IS NULL ORDER BY requested_at LIMIT $1`
	rows, err := s.Pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending workflow dispatches: %w", err)
	}
	defer rows.Close()
	var out []DispatchRequest
	for rows.Next() {
		var req DispatchRequest
		if err := rows.Scan(&req.PaymentID, &req.Scenario, &req.WorkflowID); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Postgres) MarkDispatched(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE payment_workflow_dispatch
        SET dispatched_at = COALESCE(dispatched_at, now()), last_dispatch_error = NULL WHERE payment_id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark workflow dispatched: %w", err)
	}
	return nil
}

func (s *Postgres) RecordDispatchError(ctx context.Context, id string, cause error) error {
	_, err := s.Pool.Exec(ctx, `UPDATE payment_workflow_dispatch SET last_dispatch_error = $2
        WHERE payment_id = $1 AND dispatched_at IS NULL`, id, cause.Error())
	return err
}
