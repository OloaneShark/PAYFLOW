package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/payment"
)

type Postgres struct{ Pool *pgxpool.Pool }

func (s *Postgres) CreateOrGet(ctx context.Context, candidate payment.Payment) (payment.Payment, error) {
	const insert = `INSERT INTO payments
        (id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'CREATED')
        ON CONFLICT (merchant_id, idempotency_key) DO NOTHING
        RETURNING id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status, created_at, updated_at`
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
	const selectExisting = `SELECT id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status, created_at, updated_at
        FROM payments WHERE merchant_id = $1 AND idempotency_key = $2`
	if err := scanPayment(s.Pool.QueryRow(ctx, selectExisting, candidate.MerchantID, candidate.IdempotencyKey), &p); err != nil {
		return payment.Payment{}, fmt.Errorf("find idempotent payment: %w", err)
	}
	if !bytes.Equal(p.RequestFingerprint, candidate.RequestFingerprint) || p.Amount != candidate.Amount || p.Currency != candidate.Currency {
		return payment.Payment{}, payment.ErrConflict
	}
	return p, nil
}

func (s *Postgres) Get(ctx context.Context, id string) (payment.Payment, error) {
	const query = `SELECT id, merchant_id, idempotency_key, request_fingerprint, amount, currency, status, created_at, updated_at
        FROM payments WHERE id = $1`
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
	return row.Scan(&p.ID, &p.MerchantID, &p.IdempotencyKey, &p.RequestFingerprint,
		&p.Amount, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt)
}
