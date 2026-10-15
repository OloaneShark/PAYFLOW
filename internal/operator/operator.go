package operator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

type Service struct {
	Pool      *pgxpool.Pool
	Store     *storage.Postgres
	Ledger    *ledger.Service
	Processor *payment.Processor
}

func New(pool *pgxpool.Pool) *Service {
	store := &storage.Postgres{Pool: pool}
	return &Service{Pool: pool, Store: store, Ledger: &ledger.Service{Pool: pool},
		Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool})}
}

type ProviderRecord struct {
	OperationID     string `json:"operation_id"`
	Scenario        string `json:"scenario"`
	Outcome         string `json:"outcome"`
	Attempts        int    `json:"attempts"`
	LogicalChargeID string `json:"logical_charge_id,omitempty"`
}

type DispatchRecord struct {
	WorkflowID        string     `json:"workflow_id"`
	Scenario          string     `json:"scenario"`
	RequestedAt       time.Time  `json:"requested_at"`
	DispatchedAt      *time.Time `json:"dispatched_at,omitempty"`
	LastDispatchError string     `json:"last_dispatch_error,omitempty"`
}

type Inspection struct {
	Payment  payment.Payment  `json:"payment"`
	Provider *ProviderRecord  `json:"provider,omitempty"`
	Dispatch *DispatchRecord  `json:"dispatch,omitempty"`
	Journals []ledger.Journal `json:"journals"`
}

func (s *Service) Inspect(ctx context.Context, id string) (Inspection, error) {
	if !payment.ValidPaymentID(id) {
		return Inspection{}, payment.ErrInvalidArgument
	}
	p, err := s.Store.Get(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	result := Inspection{Payment: p}
	if p.ProviderOperationID != "" {
		var r ProviderRecord
		err = s.Pool.QueryRow(ctx, `SELECT operation_id,scenario,outcome,attempts,
			COALESCE(logical_charge_id::text,'') FROM simulated_provider_operations
			WHERE operation_id=$1`, p.ProviderOperationID).
			Scan(&r.OperationID, &r.Scenario, &r.Outcome, &r.Attempts, &r.LogicalChargeID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Inspection{}, err
		}
		if err == nil {
			result.Provider = &r
		}
	}
	var d DispatchRecord
	err = s.Pool.QueryRow(ctx, `SELECT workflow_id,scenario,requested_at,dispatched_at,
		COALESCE(last_dispatch_error,'') FROM payment_workflow_dispatch WHERE payment_id=$1`, id).
		Scan(&d.WorkflowID, &d.Scenario, &d.RequestedAt, &d.DispatchedAt, &d.LastDispatchError)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Inspection{}, err
	}
	if err == nil {
		result.Dispatch = &d
	}
	result.Journals, err = s.Ledger.JournalsForPayment(ctx, id)
	return result, err
}

type Candidate struct {
	PaymentID   string     `json:"payment_id"`
	Status      string     `json:"status"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	OperationID string     `json:"operation_id"`
}

func (s *Service) Candidates(ctx context.Context, status string, limit int) ([]Candidate, error) {
	if status != payment.StatusProcessing && status != payment.StatusReconciliationRequired {
		return nil, payment.ErrInvalidArgument
	}
	if limit < 1 || limit > 1000 {
		return nil, payment.ErrInvalidArgument
	}
	rows, err := s.Pool.Query(ctx, `SELECT id,status,processing_lease_until,provider_operation_id
		FROM payments WHERE status=$1 AND ($1='RECONCILIATION_REQUIRED' OR processing_lease_until<now())
		ORDER BY updated_at LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.PaymentID, &c.Status, &c.LeaseUntil, &c.OperationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Action struct {
	ID           int64  `json:"id"`
	PaymentID    string `json:"payment_id,omitempty"`
	Action       string `json:"action"`
	State        string `json:"state"`
	BeforeStatus string `json:"before_status,omitempty"`
	AfterStatus  string `json:"after_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *Service) ActionsForPayment(ctx context.Context, id string) ([]Action, error) {
	if !payment.ValidPaymentID(id) {
		return nil, payment.ErrInvalidArgument
	}
	rows, err := s.Pool.Query(ctx, `SELECT id,action,state,COALESCE(before_status,''),
		COALESCE(after_status,''),COALESCE(error_text,'') FROM operator_actions
		WHERE payment_id=$1 ORDER BY requested_at DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		var a Action
		a.PaymentID = id
		if err := rows.Scan(&a.ID, &a.Action, &a.State, &a.BeforeStatus, &a.AfterStatus, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) beginAction(ctx context.Context, id, action, actor, before string) (Action, error) {
	var a Action
	a.PaymentID = id
	a.Action = action
	a.State = "STARTED"
	a.BeforeStatus = before
	var ref any
	if id != "" {
		ref = id
	}
	err := s.Pool.QueryRow(ctx, `INSERT INTO operator_actions (payment_id,action,actor,state,before_status)
		VALUES ($1,$2,$3,'STARTED',NULLIF($4,'')) RETURNING id`, ref, action, actor, before).Scan(&a.ID)
	return a, err
}

func (s *Service) finishAction(ctx context.Context, a *Action, after string, operationErr error) error {
	a.AfterStatus = after
	if operationErr != nil {
		a.State = "FAILED"
		a.Error = operationErr.Error()
	} else {
		a.State = "SUCCEEDED"
	}
	_, err := s.Pool.Exec(ctx, `UPDATE operator_actions SET state=$2,after_status=NULLIF($3,''),
		error_text=NULLIF($4,''),finished_at=now() WHERE id=$1`, a.ID, a.State, after, a.Error)
	return err
}

// Recover only claims an uncertain or expired payment. The Phase 2 processor
// looks up its original provider operation and owns all terminal transitions.
func (s *Service) Recover(ctx context.Context, id, actor string) (Action, error) {
	if actor == "" || !payment.ValidPaymentID(id) {
		return Action{}, payment.ErrInvalidArgument
	}
	p, err := s.Store.Get(ctx, id)
	if err != nil {
		return Action{}, err
	}
	a, err := s.beginAction(ctx, id, "RECONCILE", actor, p.Status)
	if err != nil {
		return Action{}, err
	}
	var result payment.Payment
	if !recoverable(p, time.Now()) {
		err = payment.ErrInvalidTransition
	} else {
		result, err = s.Processor.Reconcile(ctx, id)
		if err == nil && result.Status == payment.StatusProcessing {
			err = payment.ErrInvalidTransition
		}
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if auditErr := s.finishAction(finishCtx, &a, result.Status, err); auditErr != nil {
		return a, fmt.Errorf("recovery result: %v; audit update: %w", err, auditErr)
	}
	return a, err
}

func recoverable(p payment.Payment, now time.Time) bool {
	return p.Status == payment.StatusReconciliationRequired ||
		(p.Status == payment.StatusProcessing && p.ProcessingLeaseUntil != nil && p.ProcessingLeaseUntil.Before(now))
}

func (s *Service) Backfill(ctx context.Context, actor string) (Action, int, error) {
	if actor == "" {
		return Action{}, 0, payment.ErrInvalidArgument
	}
	a, err := s.beginAction(ctx, "", "BACKFILL", actor, "")
	if err != nil {
		return Action{}, 0, err
	}
	count, err := s.Ledger.Backfill(ctx)
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if auditErr := s.finishAction(finishCtx, &a, "", err); auditErr != nil {
		return a, count, fmt.Errorf("backfill result: %v; audit update: %w", err, auditErr)
	}
	return a, count, err
}
