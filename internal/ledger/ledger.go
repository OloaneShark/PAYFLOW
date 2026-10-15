package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ApprovalPurpose = "PAYMENT_APPROVAL"

var (
	ErrNotApproved = errors.New("payment has no matching durable provider approval")
	ErrConflict    = errors.New("existing approval journal conflicts with payment")
)

type Journal struct {
	ID                  int64     `json:"id"`
	PaymentID           string    `json:"payment_id"`
	Purpose             string    `json:"purpose"`
	PostingReference    string    `json:"posting_reference"`
	ProviderOperationID string    `json:"provider_operation_id"`
	MerchantID          string    `json:"merchant_id"`
	Currency            string    `json:"currency"`
	Amount              int64     `json:"amount"`
	PostedAt            time.Time `json:"posted_at"`
	Entries             []Entry   `json:"entries,omitempty"`
}

type Entry struct {
	ID        int64     `json:"id"`
	JournalID int64     `json:"journal_id"`
	AccountID int64     `json:"account_id"`
	Side      string    `json:"side"`
	Currency  string    `json:"currency"`
	Amount    int64     `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

type Balance struct {
	AccountID  int64  `json:"account_id"`
	Kind       string `json:"kind"`
	MerchantID string `json:"merchant_id,omitempty"`
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount"`
}

type Issue struct {
	PaymentID string `json:"payment_id"`
	Reason    string `json:"reason"`
}

type Service struct{ Pool *pgxpool.Pool }

type paymentFacts struct {
	id, merchantID, currency, status, operationID string
	amount                                        int64
}

// PostApprovalTx is called inside the same transaction that marks a payment
// COMPLETED. It locks the payment, verifies the durable provider decision, and
// inserts at most one journal. The deferred database trigger checks its entries
// and final payment status when the outer transaction commits.
func PostApprovalTx(ctx context.Context, tx pgx.Tx, paymentID string) (Journal, error) {
	var p paymentFacts
	err := tx.QueryRow(ctx, `SELECT id, merchant_id, currency, status, amount,
		COALESCE(provider_operation_id::text, '') FROM payments WHERE id=$1 FOR UPDATE`, paymentID).
		Scan(&p.id, &p.merchantID, &p.currency, &p.status, &p.amount, &p.operationID)
	if err != nil {
		return Journal{}, fmt.Errorf("lock payment for ledger: %w", err)
	}
	if p.amount <= 0 || p.operationID == "" || (p.status != "PROCESSING" && p.status != "COMPLETED") {
		return Journal{}, ErrNotApproved
	}
	var outcome string
	err = tx.QueryRow(ctx, `SELECT outcome FROM simulated_provider_operations
		WHERE operation_id=$1 AND payment_id=$2`, p.operationID, p.id).Scan(&outcome)
	if err != nil || outcome != "APPROVED" {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Journal{}, fmt.Errorf("verify provider approval: %w", err)
		}
		return Journal{}, ErrNotApproved
	}
	if existing, found, err := journalByPayment(ctx, tx, p.id); err != nil {
		return Journal{}, err
	} else if found {
		if existing.ProviderOperationID != p.operationID || existing.MerchantID != p.merchantID ||
			existing.Currency != p.currency || existing.Amount != p.amount ||
			existing.PostingReference != "approval:"+p.id || existing.Purpose != ApprovalPurpose {
			return Journal{}, ErrConflict
		}
		return existing, nil
	}
	providerAccount, err := accountID(ctx, tx, "PROVIDER_RECEIVABLE", "", p.currency)
	if err != nil {
		return Journal{}, err
	}
	merchantAccount, err := accountID(ctx, tx, "MERCHANT_PAYABLE", p.merchantID, p.currency)
	if err != nil {
		return Journal{}, err
	}
	var j Journal
	err = tx.QueryRow(ctx, `INSERT INTO ledger_journals
		(payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
		VALUES ($1,'PAYMENT_APPROVAL',$2,$3,$4,$5,$6)
		RETURNING id,payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount,posted_at`,
		p.id, "approval:"+p.id, p.operationID, p.merchantID, p.currency, p.amount).Scan(
		&j.ID, &j.PaymentID, &j.Purpose, &j.PostingReference, &j.ProviderOperationID,
		&j.MerchantID, &j.Currency, &j.Amount, &j.PostedAt)
	if err != nil {
		return Journal{}, fmt.Errorf("insert approval journal: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries (journal_id,account_id,side,currency,amount)
		VALUES ($1,$2,'DEBIT',$4,$5),($1,$3,'CREDIT',$4,$5)`,
		j.ID, providerAccount, merchantAccount, p.currency, p.amount)
	if err != nil {
		return Journal{}, fmt.Errorf("insert approval entries: %w", err)
	}
	return j, nil
}

func accountID(ctx context.Context, tx pgx.Tx, kind, merchantID, currency string) (int64, error) {
	var owner any
	if kind == "MERCHANT_PAYABLE" {
		owner = merchantID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (kind,merchant_id,currency)
		VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, kind, owner, currency); err != nil {
		return 0, fmt.Errorf("create ledger account: %w", err)
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts
		WHERE kind=$1 AND merchant_id IS NOT DISTINCT FROM $2 AND currency=$3`, kind, owner, currency).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("find ledger account: %w", err)
	}
	return id, nil
}

const journalColumns = `id,payment_id,purpose,posting_reference,provider_operation_id,
	merchant_id,currency,amount,posted_at`

func scanJournal(row pgx.Row, j *Journal) error {
	return row.Scan(&j.ID, &j.PaymentID, &j.Purpose, &j.PostingReference,
		&j.ProviderOperationID, &j.MerchantID, &j.Currency, &j.Amount, &j.PostedAt)
}

func journalByPayment(ctx context.Context, tx pgx.Tx, id string) (Journal, bool, error) {
	var j Journal
	err := scanJournal(tx.QueryRow(ctx, `SELECT `+journalColumns+` FROM ledger_journals
		WHERE payment_id=$1 AND purpose='PAYMENT_APPROVAL'`, id), &j)
	if errors.Is(err, pgx.ErrNoRows) {
		return Journal{}, false, nil
	}
	if err != nil {
		return Journal{}, false, fmt.Errorf("find approval journal: %w", err)
	}
	return j, true, nil
}

// PostApproval is used by the historical backfill. A new completion uses
// PostApprovalTx so that journal insertion and status change commit together.
func (s *Service) PostApproval(ctx context.Context, id string) (Journal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Journal{}, err
	}
	defer tx.Rollback(ctx)
	j, err := PostApprovalTx(ctx, tx, id)
	if err != nil {
		return Journal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Journal{}, fmt.Errorf("commit approval journal: %w", err)
	}
	return j, nil
}

func (s *Service) JournalsForPayment(ctx context.Context, id string) ([]Journal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+journalColumns+` FROM ledger_journals WHERE payment_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	journals := []Journal{}
	for rows.Next() {
		var j Journal
		if err := scanJournal(rows, &j); err != nil {
			rows.Close()
			return nil, err
		}
		journals = append(journals, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range journals {
		journals[i].Entries, err = s.Entries(ctx, journals[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return journals, nil
}

func (s *Service) Entries(ctx context.Context, journalID int64) ([]Entry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,journal_id,account_id,side,currency,amount,created_at
		FROM ledger_entries WHERE journal_id=$1 ORDER BY id`, journalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.JournalID, &e.AccountID, &e.Side, &e.Currency, &e.Amount, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *Service) Balance(ctx context.Context, kind, merchantID, currency string) (Balance, error) {
	var b Balance
	err := s.Pool.QueryRow(ctx, `SELECT a.id,a.kind,COALESCE(a.merchant_id,''),a.currency,
		COALESCE(SUM(CASE WHEN e.side='DEBIT' THEN e.amount ELSE -e.amount END),0)
		* CASE WHEN a.kind='MERCHANT_PAYABLE' THEN -1 ELSE 1 END
		FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id=a.id
		WHERE a.kind=$1 AND a.merchant_id IS NOT DISTINCT FROM NULLIF($2,'') AND a.currency=$3
		GROUP BY a.id`, kind, merchantID, currency).
		Scan(&b.AccountID, &b.Kind, &b.MerchantID, &b.Currency, &b.Amount)
	if err != nil {
		return Balance{}, err
	}
	return b, nil
}

// CompletedIssues detects historical omissions and any inconsistent journal
// visible despite the database posting guards. It is read-only.
func (s *Service) CompletedIssues(ctx context.Context, limit int) ([]Issue, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id,
		CASE WHEN o.outcome IS DISTINCT FROM 'APPROVED' OR o.payment_id IS DISTINCT FROM p.id
			THEN 'provider approval missing or inconsistent'
		WHEN j.id IS NULL THEN 'approval journal missing'
		WHEN j.provider_operation_id IS DISTINCT FROM p.provider_operation_id
			OR j.amount IS DISTINCT FROM p.amount OR j.currency IS DISTINCT FROM p.currency
			OR j.merchant_id IS DISTINCT FROM p.merchant_id THEN 'approval journal metadata mismatch'
		WHEN coalesce(x.entry_count,0) <> 2 OR x.debits IS DISTINCT FROM p.amount
			OR x.credits IS DISTINCT FROM p.amount THEN 'approval journal entries unbalanced'
		ELSE '' END AS reason
		FROM payments p
		LEFT JOIN simulated_provider_operations o ON o.operation_id=p.provider_operation_id
		LEFT JOIN ledger_journals j ON j.payment_id=p.id AND j.purpose='PAYMENT_APPROVAL'
		LEFT JOIN LATERAL (
			SELECT count(*) AS entry_count,
				coalesce(sum(amount) FILTER (WHERE side='DEBIT'),0) AS debits,
				coalesce(sum(amount) FILTER (WHERE side='CREDIT'),0) AS credits
			FROM ledger_entries WHERE journal_id=j.id
		) x ON true
		WHERE p.status='COMPLETED' ORDER BY p.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		var issue Issue
		if err := rows.Scan(&issue.PaymentID, &issue.Reason); err != nil {
			return nil, err
		}
		if issue.Reason != "" {
			issues = append(issues, issue)
		}
		if len(issues) >= limit {
			break
		}
	}
	return issues, rows.Err()
}

// Backfill scans completed payments, verifies provider decisions, and posts
// missing journals one payment at a time. Rerunning it is safe.
func (s *Service) Backfill(ctx context.Context) (int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id FROM payments p LEFT JOIN ledger_journals j
		ON j.payment_id=p.id AND j.purpose='PAYMENT_APPROVAL'
		WHERE p.status='COMPLETED' AND j.id IS NULL ORDER BY p.created_at`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if _, err := s.PostApproval(ctx, id); err != nil {
			return count, fmt.Errorf("backfill payment %s: %w", id, err)
		}
		count++
	}
	return count, nil
}
