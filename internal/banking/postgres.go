package banking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/observability"
)

// Postgres persists provider decisions independently of payment status updates.
// An approved operation has exactly one stable logical_charge_id.
type Postgres struct {
	Pool    *pgxpool.Pool
	Metrics *observability.Registry
}

func (p *Postgres) Execute(ctx context.Context, op Operation) (Outcome, error) {
	started := time.Now()
	defer func() {
		if p.Metrics != nil {
			p.Metrics.Observe("payflow_provider_response_seconds", time.Since(started))
		}
	}()
	if !op.Scenario.Valid() {
		return Unknown, fmt.Errorf("invalid provider scenario %q", op.Scenario)
	}
	initial := Unknown
	if op.Scenario == Approve || op.Scenario == LostApprovalResponse {
		initial = Approved
	} else if op.Scenario == Decline {
		initial = Declined
	}
	var chargeID any
	if initial == Approved {
		chargeID = op.ID
	}
	// The operation ID is the provider's idempotency key. Upsert serializes
	// concurrent calls and never creates a second row or charge identifier.
	const upsert = `INSERT INTO simulated_provider_operations
        (operation_id, payment_id, scenario, outcome, logical_charge_id)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (operation_id) DO UPDATE
        SET attempts = simulated_provider_operations.attempts + 1,
            updated_at = now()
        RETURNING payment_id, scenario, outcome, attempts`
	var paymentID string
	var scenario Scenario
	var outcome Outcome
	var attempts int
	if err := p.Pool.QueryRow(ctx, upsert, op.ID, op.PaymentID, op.Scenario, initial, chargeID).
		Scan(&paymentID, &scenario, &outcome, &attempts); err != nil {
		if p.Metrics != nil {
			p.Metrics.Inc("payflow_database_errors_total")
		}
		return Unknown, fmt.Errorf("record simulated provider operation: %w", err)
	}
	if paymentID != op.PaymentID || scenario != op.Scenario {
		return Unknown, errors.New("provider operation ID reused with different payment or scenario")
	}
	if outcome == Approved || outcome == Declined {
		slog.InfoContext(ctx, "mock provider decision", "payment_id", op.PaymentID, "provider_operation_id", op.ID, "outcome", outcome)
		if op.Scenario == LostApprovalResponse && attempts == 1 {
			return Unknown, ErrLostResponse
		}
		return outcome, nil
	}
	switch op.Scenario {
	case TemporaryFailure:
		if attempts == 1 {
			return Unknown, ErrTemporary
		}
		const approve = `UPDATE simulated_provider_operations
            SET outcome = 'APPROVED', logical_charge_id = operation_id, updated_at = now()
            WHERE operation_id = $1 AND outcome = 'PENDING'
            RETURNING outcome`
		if err := p.Pool.QueryRow(ctx, approve, op.ID).Scan(&outcome); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return p.Lookup(ctx, op)
			}
			if p.Metrics != nil {
				p.Metrics.Inc("payflow_database_errors_total")
			}
			return Unknown, fmt.Errorf("resolve temporary provider failure: %w", err)
		}
		slog.InfoContext(ctx, "mock provider decision", "payment_id", op.PaymentID, "provider_operation_id", op.ID, "outcome", outcome)
		return outcome, nil
	case NetworkTimeout:
		return Unknown, ErrTimeout
	default:
		return Unknown, errors.New("unexpected pending provider operation")
	}
}

func (p *Postgres) Lookup(ctx context.Context, op Operation) (Outcome, error) {
	started := time.Now()
	defer func() {
		if p.Metrics != nil {
			p.Metrics.Observe("payflow_provider_response_seconds", time.Since(started))
		}
	}()
	const query = `SELECT payment_id, scenario, outcome FROM simulated_provider_operations WHERE operation_id = $1`
	var paymentID string
	var scenario Scenario
	var outcome Outcome
	if err := p.Pool.QueryRow(ctx, query, op.ID).Scan(&paymentID, &scenario, &outcome); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Unknown, nil
		}
		if p.Metrics != nil {
			p.Metrics.Inc("payflow_database_errors_total")
		}
		return Unknown, fmt.Errorf("lookup provider operation: %w", err)
	}
	if paymentID != op.PaymentID || scenario != op.Scenario {
		return Unknown, errors.New("provider operation ID reused with different payment or scenario")
	}
	if outcome == Approved || outcome == Declined {
		slog.InfoContext(ctx, "mock provider decision lookup", "payment_id", op.PaymentID, "provider_operation_id", op.ID, "outcome", outcome)
	}
	return outcome, nil
}
