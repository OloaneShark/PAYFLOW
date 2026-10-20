package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/ledger"
	"payflow/internal/observability"
	"payflow/internal/payment"
)

type Processor interface {
	Reconcile(context.Context, string) (payment.Payment, error)
}

type Scanner struct {
	Pool        *pgxpool.Pool
	Processor   Processor
	Logger      *slog.Logger
	Interval    time.Duration
	MaxInterval time.Duration
	MaxAttempts int
	BatchSize   int
	Metrics     *observability.Registry
}

// Scan claims only due rows, using SKIP LOCKED so independent workers do not
// schedule the same candidate at once. The network call happens after commit.
// A worker crash leaves a finite backoff; the original operation ID is intact.
func (s *Scanner) Scan(ctx context.Context) (int, error) {
	if s.Interval <= 0 || s.MaxAttempts <= 0 || s.BatchSize <= 0 {
		return 0, errors.New("invalid recovery configuration")
	}
	maxInterval := s.MaxInterval
	if maxInterval == 0 {
		maxInterval = 5 * time.Minute
	}
	if maxInterval < s.Interval {
		return 0, errors.New("recovery max interval must be >= interval")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `WITH due AS (
        SELECT id FROM payments
        WHERE ((status='PROCESSING' AND processing_lease_until < now())
            OR status='RECONCILIATION_REQUIRED')
          AND recovery_attempts < $1
          AND (recovery_next_attempt_at IS NULL OR recovery_next_attempt_at <= now())
        ORDER BY updated_at LIMIT $2 FOR UPDATE SKIP LOCKED
    ) UPDATE payments p SET recovery_attempts=p.recovery_attempts+1,
		recovery_next_attempt_at=now() + LEAST($3::bigint * (1::bigint << LEAST(p.recovery_attempts,10)),$4::bigint) * interval '1 millisecond'
	FROM due WHERE p.id=due.id RETURNING p.id,p.status`, s.MaxAttempts, s.BatchSize, s.Interval.Milliseconds(), maxInterval.Milliseconds())
	if err != nil {
		if s.Metrics != nil {
			s.Metrics.Inc("payflow_database_errors_total")
		}
		return 0, fmt.Errorf("scan abandoned claims: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		if status == payment.StatusProcessing && s.Metrics != nil {
			s.Metrics.Inc("payflow_abandoned_claims_total")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.RefreshMetrics(ctx)
	var failures []error
	for _, id := range ids {
		// A single failed database operation does not block unrelated claims.
		started := time.Now()
		itemCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		p, err := s.Processor.Reconcile(itemCtx, id)
		cancel()
		if err != nil {
			s.Logger.Warn("payment recovery failed", "payment_id", id, "error_category", "reconcile", "duration_ms", time.Since(started).Milliseconds())
			failures = append(failures, err)
		} else {
			s.Logger.Info("payment recovery attempt", "payment_id", id, "status", p.Status, "duration_ms", time.Since(started).Milliseconds())
		}
	}
	return len(ids), errors.Join(failures...)
}

func (s *Scanner) RefreshMetrics(ctx context.Context) {
	if s.Metrics == nil {
		return
	}
	var backlog, stuck, exhausted int64
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='RECONCILIATION_REQUIRED'),
		count(*) FILTER (WHERE status='PROCESSING' AND processing_lease_until<now()),
		count(*) FILTER (WHERE (status='RECONCILIATION_REQUIRED' OR
			(status='PROCESSING' AND processing_lease_until<now())) AND recovery_attempts >= $1)
		FROM payments`, s.MaxAttempts).Scan(&backlog, &stuck, &exhausted)
	if err != nil {
		s.Metrics.Inc("payflow_database_errors_total")
		s.Metrics.Set("payflow_database_connectivity", 0)
		return
	}
	s.Metrics.Set("payflow_database_connectivity", 1)
	s.Metrics.Set("payflow_reconciliation_backlog", float64(backlog))
	s.Metrics.Set("payflow_stuck_processing_claims", float64(stuck))
	s.Metrics.Set("payflow_reconciliation_retry_exhausted", float64(exhausted))
	var dispatchBacklog int64
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM payment_workflow_dispatch WHERE dispatched_at IS NULL`).Scan(&dispatchBacklog); err != nil {
		s.Metrics.Inc("payflow_database_errors_total")
		s.Metrics.Set("payflow_database_connectivity", 0)
		return
	}
	s.Metrics.Set("payflow_workflow_dispatch_backlog", float64(dispatchBacklog))
	issues, err := (&ledger.Service{Pool: s.Pool}).CompletedIssues(ctx, 10000)
	if err != nil {
		s.Metrics.Inc("payflow_database_errors_total")
		return
	}
	s.Metrics.Set("payflow_ledger_integrity_violations", float64(len(issues)))
	if len(issues) > 0 {
		s.Metrics.Inc("payflow_ledger_invariant_violations_total")
	}
}

func (s *Scanner) Run(ctx context.Context) {
	for ctx.Err() == nil {
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.Scan(scanCtx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			s.Logger.Warn("recovery scan failed", "error_category", "recovery")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.Interval):
		}
	}
}
