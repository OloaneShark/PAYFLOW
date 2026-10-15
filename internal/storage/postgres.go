package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/payment"
)

type Postgres struct{ Pool *pgxpool.Pool }

const paymentColumns = `id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status,
    created_at, updated_at, COALESCE(provider_operation_id::text, ''), COALESCE(processing_scenario, ''),
    processing_started_at, completed_at, failed_at, reconciliation_required_at, processing_lease_until`

func (s *Postgres) CreateOrGet(ctx context.Context, candidate payment.Payment) (payment.Payment, error) {
	const insert = `INSERT INTO payments
        (id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'CREATED')
        ON CONFLICT (merchant_id, idempotency_key) DO NOTHING
		RETURNING ` + paymentColumns
	var p payment.Payment
	err := scanPayment(s.Pool.QueryRow(ctx, insert, candidate.ID, candidate.MerchantID, candidate.IdempotencyKey,
		candidate.RequestFingerprint, candidate.Amount, candidate.Currency), &p)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	// A second statement gets a fresh READ COMMITTED snapshot. PostgreSQL's
	// unique index makes the INSERT wait for a concurrent writer to commit.
	const selectExisting = `SELECT ` + paymentColumns + ` FROM payments WHERE merchant_id = $1 AND idempotency_key = $2`
	if err := scanPayment(s.Pool.QueryRow(ctx, selectExisting, candidate.MerchantID, candidate.IdempotencyKey), &p); err != nil {
		return payment.Payment{}, fmt.Errorf("find idempotent payment: %w", err)
	}
	if !bytes.Equal(p.RequestFingerprint, candidate.RequestFingerprint) || p.Amount != candidate.Amount || p.Currency != candidate.Currency {
		return payment.Payment{}, payment.ErrConflict
	}
	return p, nil
}

func (s *Postgres) Get(ctx context.Context, id string) (payment.Payment, error) {
	const query = `SELECT ` + paymentColumns + ` FROM payments WHERE id = $1`
	var p payment.Payment
	if err := scanPayment(s.Pool.QueryRow(ctx, query, id), &p); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return payment.Payment{}, payment.ErrNotFound
		}
		return payment.Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return p, nil
}

func scanPayment(row pgx.Row, p *payment.Payment) error {
	var started, completed, failed, reconciliation, lease pgtype.Timestamptz
	if err := row.Scan(&p.ID, &p.MerchantID, &p.IdempotencyKey, &p.RequestFingerprint,
		&p.Amount, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt,
		&p.ProviderOperationID, &p.ProcessingScenario, &started, &completed, &failed, &reconciliation, &lease); err != nil {
		return err
	}
	if started.Valid {
		p.ProcessingStartedAt = &started.Time
	}
	if completed.Valid {
		p.CompletedAt = &completed.Time
	}
	if failed.Valid {
		p.FailedAt = &failed.Time
	}
	if reconciliation.Valid {
		p.ReconciliationRequiredAt = &reconciliation.Time
	}
	if lease.Valid {
		p.ProcessingLeaseUntil = &lease.Time
	}
	return nil
}

func (s *Postgres) Claim(ctx context.Context, id string, scenario banking.Scenario, operationID, token string, lease time.Duration) (payment.Payment, bool, error) {
	query := `UPDATE payments SET status = 'PROCESSING', provider_operation_id = $2,
		processing_scenario = $3, processing_token = $4, processing_lease_until = now() + ($5 * interval '1 millisecond'),
        processing_started_at = now(), updated_at = now()
        WHERE id = $1 AND status = 'CREATED' RETURNING ` + paymentColumns
	var p payment.Payment
	err := scanPayment(s.Pool.QueryRow(ctx, query, id, operationID, scenario, token, lease.Milliseconds()), &p)
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, false, fmt.Errorf("claim payment: %w", err)
	}
	p, err = s.Get(ctx, id)
	if err != nil {
		return payment.Payment{}, false, err
	}
	if p.ProcessingScenario != scenario {
		return payment.Payment{}, false, payment.ErrScenarioConflict
	}
	return p, false, nil
}

func (s *Postgres) ClaimReconciliation(ctx context.Context, id, token string, lease time.Duration) (payment.Payment, bool, error) {
	query := `UPDATE payments SET status = 'PROCESSING', processing_token = $2,
		processing_lease_until = now() + ($3 * interval '1 millisecond'), updated_at = now()
        WHERE id = $1 AND (status = 'RECONCILIATION_REQUIRED'
            OR (status = 'PROCESSING' AND processing_lease_until < now()))
        RETURNING ` + paymentColumns
	var p payment.Payment
	err := scanPayment(s.Pool.QueryRow(ctx, query, id, token, lease.Milliseconds()), &p)
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, false, fmt.Errorf("claim reconciliation: %w", err)
	}
	p, err = s.Get(ctx, id)
	if err != nil {
		return payment.Payment{}, false, err
	}
	if p.Status == payment.StatusCreated {
		return payment.Payment{}, false, payment.ErrInvalidTransition
	}
	return p, false, nil
}

func (s *Postgres) Finish(ctx context.Context, id, token, target, reason string) (payment.Payment, error) {
	if !payment.ValidTransition(payment.StatusProcessing, target) {
		return payment.Payment{}, payment.ErrInvalidTransition
	}
	if target == payment.StatusCompleted {
		return s.finishCompleted(ctx, id, token)
	}
	query := `UPDATE payments SET status = $3, processing_token = NULL, processing_lease_until = NULL,
        completed_at = CASE WHEN $3 = 'COMPLETED' THEN now() ELSE completed_at END,
        failed_at = CASE WHEN $3 = 'FAILED' THEN now() ELSE failed_at END,
        reconciliation_required_at = CASE WHEN $3 = 'RECONCILIATION_REQUIRED' THEN now() ELSE reconciliation_required_at END,
        last_processing_error = NULLIF($4, ''), updated_at = now()
        WHERE id = $1 AND status = 'PROCESSING' AND processing_token = $2
        RETURNING ` + paymentColumns
	var p payment.Payment
	err := scanPayment(s.Pool.QueryRow(ctx, query, id, token, target, reason), &p)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrInvalidTransition
	}
	if err != nil {
		return payment.Payment{}, fmt.Errorf("finish payment: %w", err)
	}
	return p, nil
}

func (s *Postgres) finishCompleted(ctx context.Context, id, token string) (payment.Payment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return payment.Payment{}, fmt.Errorf("begin payment completion: %w", err)
	}
	defer tx.Rollback(ctx)
	var status, claimToken string
	err = tx.QueryRow(ctx, `SELECT status, COALESCE(processing_token::text,'')
		FROM payments WHERE id=$1 FOR UPDATE`, id).Scan(&status, &claimToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrNotFound
	}
	if err != nil {
		return payment.Payment{}, fmt.Errorf("lock payment completion: %w", err)
	}
	if status != payment.StatusProcessing || claimToken != token {
		return payment.Payment{}, payment.ErrInvalidTransition
	}
	if _, err := ledger.PostApprovalTx(ctx, tx, id); err != nil {
		return payment.Payment{}, fmt.Errorf("post approval journal: %w", err)
	}
	query := `UPDATE payments SET status='COMPLETED', processing_token=NULL,
		processing_lease_until=NULL, completed_at=now(), last_processing_error=NULL, updated_at=now()
		WHERE id=$1 AND status='PROCESSING' AND processing_token=$2
		RETURNING ` + paymentColumns
	var result payment.Payment
	if err := scanPayment(tx.QueryRow(ctx, query, id, token), &result); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return payment.Payment{}, payment.ErrInvalidTransition
		}
		return payment.Payment{}, fmt.Errorf("finish approved payment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.Payment{}, fmt.Errorf("commit payment completion and journal: %w", err)
	}
	return result, nil
}
