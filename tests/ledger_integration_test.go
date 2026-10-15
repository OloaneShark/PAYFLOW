package tests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/operator"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func ledgerPayment(t *testing.T, pool *pgxpool.Pool, merchant string) payment.Payment {
	t.Helper()
	p, err := payment.NewService(&storage.Postgres{Pool: pool}).Create(context.Background(), payment.CreateInput{
		MerchantID: merchant, IdempotencyKey: fmt.Sprintf("ledger-%d", time.Now().UnixNano()), Amount: 950, Currency: "JPY",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func journalCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM ledger_journals WHERE payment_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLedgerApprovalBalancesAndDuplicatePosting(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	provider := &banking.Postgres{Pool: pool}
	processor := payment.NewProcessor(store, provider)
	accounts := &ledger.Service{Pool: pool}
	merchantA := fmt.Sprintf("merchant-a-%d", time.Now().UnixNano())
	merchantB := fmt.Sprintf("merchant-b-%d", time.Now().UnixNano())
	first := ledgerPayment(t, pool, merchantA)
	second := ledgerPayment(t, pool, merchantB)
	for _, p := range []payment.Payment{first, second} {
		completed, err := processor.Process(context.Background(), p.ID, banking.Approve)
		if err != nil || completed.Status != payment.StatusCompleted {
			t.Fatalf("complete %s: %+v, %v", p.ID, completed, err)
		}
		journals, err := accounts.JournalsForPayment(context.Background(), p.ID)
		if err != nil || len(journals) != 1 {
			t.Fatalf("journals = %+v, %v", journals, err)
		}
		j := journals[0]
		if j.PaymentID != p.ID || j.Amount != 950 || j.Currency != "JPY" || len(j.Entries) != 2 ||
			j.Entries[0].Side != "DEBIT" || j.Entries[1].Side != "CREDIT" ||
			j.Entries[0].Amount != 950 || j.Entries[1].Amount != 950 {
			t.Fatalf("unbalanced journal = %+v", j)
		}
		for i := 0; i < 3; i++ {
			repeated, err := accounts.PostApproval(context.Background(), p.ID)
			if err != nil || repeated.ID != j.ID {
				t.Fatalf("duplicate posting = %+v, %v", repeated, err)
			}
		}
		if journalCount(t, pool, p.ID) != 1 {
			t.Fatal("duplicate journal")
		}
	}
	a, err := accounts.Balance(context.Background(), "MERCHANT_PAYABLE", merchantA, "JPY")
	if err != nil || a.Amount != 950 {
		t.Fatalf("merchant A balance = %+v, %v", a, err)
	}
	b, err := accounts.Balance(context.Background(), "MERCHANT_PAYABLE", merchantB, "JPY")
	if err != nil || b.Amount != 950 || b.AccountID == a.AccountID {
		t.Fatalf("merchant B balance = %+v, %v", b, err)
	}
	providerBalance, err := accounts.Balance(context.Background(), "PROVIDER_RECEIVABLE", "", "JPY")
	if err != nil || providerBalance.Amount < 1900 {
		t.Fatalf("provider receivable = %+v, %v", providerBalance, err)
	}
	issues, err := accounts.CompletedIssues(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.PaymentID == first.ID || issue.PaymentID == second.ID {
			t.Fatalf("new journal issue: %+v", issue)
		}
	}
}

func TestLedgerConcurrentActivityOneJournal(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	p := ledgerPayment(t, pool, fmt.Sprintf("concurrent-ledger-%d", time.Now().UnixNano()))
	a := &orchestration.Activities{Store: store, Processor: payment.NewProcessor(store, &banking.Postgres{Pool: pool})}
	const n = 16
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Advance(context.Background(), p.ID, banking.Approve)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	completed, err := store.Get(context.Background(), p.ID)
	if err != nil || completed.Status != payment.StatusCompleted {
		t.Fatalf("payment = %+v, %v", completed, err)
	}
	if journalCount(t, pool, p.ID) != 1 {
		t.Fatal("concurrent activities created more than one journal")
	}
	_, attempts, charges := providerFacts(t, pool, p.ID)
	if attempts != 1 || charges != 1 {
		t.Fatalf("provider attempts=%d, charges=%d", attempts, charges)
	}
}

func TestLedgerDeclineAndUnknownDoNotPost(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	merchant := fmt.Sprintf("nonapproval-%d", time.Now().UnixNano())
	declined := ledgerPayment(t, pool, merchant)
	if p, err := processor.Process(context.Background(), declined.ID, banking.Decline); err != nil || p.Status != payment.StatusFailed {
		t.Fatalf("decline = %+v,%v", p, err)
	}
	if journalCount(t, pool, declined.ID) != 0 {
		t.Fatal("decline posted an approval journal")
	}
	timeout := ledgerPayment(t, pool, merchant)
	if p, err := processor.Process(context.Background(), timeout.ID, banking.NetworkTimeout); err != nil || p.Status != payment.StatusReconciliationRequired {
		t.Fatalf("timeout = %+v,%v", p, err)
	}
	if journalCount(t, pool, timeout.ID) != 0 {
		t.Fatal("unknown outcome posted an approval journal")
	}
	lost := ledgerPayment(t, pool, merchant)
	if p, err := processor.Process(context.Background(), lost.ID, banking.LostApprovalResponse); err != nil || p.Status != payment.StatusReconciliationRequired {
		t.Fatalf("lost = %+v,%v", p, err)
	}
	if journalCount(t, pool, lost.ID) != 0 {
		t.Fatal("lost response posted before reconciliation")
	}
	if p, err := processor.Reconcile(context.Background(), lost.ID); err != nil || p.Status != payment.StatusCompleted {
		t.Fatalf("reconcile = %+v,%v", p, err)
	}
	if journalCount(t, pool, lost.ID) != 1 {
		t.Fatal("confirmed approval was not posted")
	}
}

func TestLedgerRejectsInvalidAndImmutablePosting(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	p := ledgerPayment(t, pool, fmt.Sprintf("invalid-ledger-%d", time.Now().UnixNano()))
	claimed, ok, err := store.Claim(context.Background(), p.ID, banking.Approve, p.ID, "123e4567-e89b-42d3-a456-426614174021", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %+v,%v", claimed, err)
	}
	if _, err := (&banking.Postgres{Pool: pool}).Execute(context.Background(), banking.Operation{ID: p.ID, PaymentID: p.ID, Scenario: banking.Approve}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET status='COMPLETED',completed_at=now(),processing_token=NULL,processing_lease_until=NULL WHERE id=$1`, p.ID); err == nil {
		t.Fatal("database accepted completion without journal")
	}
	for _, amount := range []int64{0, -1} {
		_, err := pool.Exec(context.Background(), `INSERT INTO ledger_journals (payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
			VALUES ($1,'PAYMENT_APPROVAL',$2,$1,$3,'JPY',$4)`, p.ID, "approval:"+p.ID, p.MerchantID, amount)
		if err == nil {
			t.Fatalf("database accepted journal amount %d", amount)
		}
	}
	for _, amount := range []int64{0, -1} {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var journalID int64
		err = tx.QueryRow(context.Background(), `INSERT INTO ledger_journals
			(payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
			VALUES ($1,'PAYMENT_APPROVAL',$2,$1,$3,'JPY',950) RETURNING id`, p.ID, "approval:"+p.ID, p.MerchantID).Scan(&journalID)
		if err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), `INSERT INTO ledger_accounts(kind,currency)
			VALUES ('PROVIDER_RECEIVABLE','JPY') ON CONFLICT DO NOTHING`); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatal(err)
		}
		_, err = tx.Exec(context.Background(), `INSERT INTO ledger_entries(journal_id,account_id,side,currency,amount)
			SELECT $1,id,'DEBIT','JPY',$2 FROM ledger_accounts WHERE kind='PROVIDER_RECEIVABLE' AND currency='JPY'`, journalID, amount)
		_ = tx.Rollback(context.Background())
		if err == nil {
			t.Fatalf("database accepted entry amount %d", amount)
		}
	}
	// A two-entry journal with unequal sides is rejected at commit. The
	// payment transition in that transaction must roll back with it.
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `INSERT INTO ledger_accounts(kind,currency)
		VALUES ('PROVIDER_RECEIVABLE','JPY') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `INSERT INTO ledger_accounts(kind,merchant_id,currency)
		VALUES ('MERCHANT_PAYABLE',$1,'JPY') ON CONFLICT DO NOTHING`, p.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	var journalID int64
	err = tx.QueryRow(context.Background(), `INSERT INTO ledger_journals
		(payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
		VALUES ($1,'PAYMENT_APPROVAL',$2,$1,$3,'JPY',950) RETURNING id`, p.ID, "approval:"+p.ID, p.MerchantID).Scan(&journalID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `INSERT INTO ledger_entries(journal_id,account_id,side,currency,amount)
		SELECT $1,id,'DEBIT','JPY',950 FROM ledger_accounts WHERE kind='PROVIDER_RECEIVABLE' AND currency='JPY'`, journalID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `INSERT INTO ledger_entries(journal_id,account_id,side,currency,amount)
		SELECT $1,id,'CREDIT','JPY',949 FROM ledger_accounts WHERE kind='MERCHANT_PAYABLE' AND merchant_id=$2 AND currency='JPY'`, journalID, p.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `UPDATE payments SET status='COMPLETED',completed_at=now(),processing_token=NULL,processing_lease_until=NULL WHERE id=$1`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err == nil {
		t.Fatal("unbalanced journal committed")
	}
	if journalCount(t, pool, p.ID) != 0 {
		t.Fatal("partial journal survived failed commit")
	}
	if persisted, err := store.Get(context.Background(), p.ID); err != nil || persisted.Status != payment.StatusProcessing {
		t.Fatalf("payment escaped rollback: %+v,%v", persisted, err)
	}
	// The deferred constraint rejects a journal with no entries at commit.
	_, err = pool.Exec(context.Background(), `INSERT INTO ledger_journals (payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
		VALUES ($1,'PAYMENT_APPROVAL',$2,$1,$3,'JPY',950)`, p.ID, "approval:"+p.ID, p.MerchantID)
	if err == nil || journalCount(t, pool, p.ID) != 0 {
		t.Fatalf("database accepted incomplete journal: %v", err)
	}
	completed, err := payment.NewProcessor(store, &banking.Postgres{Pool: pool}).Reconcile(context.Background(), p.ID)
	if err == nil && completed.Status == payment.StatusCompleted {
		t.Fatal("active claim was overridden")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	completed, err = payment.NewProcessor(store, &banking.Postgres{Pool: pool}).Reconcile(context.Background(), p.ID)
	if err != nil || completed.Status != payment.StatusCompleted {
		t.Fatalf("completion = %+v,%v", completed, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE simulated_provider_operations
		SET outcome='PENDING',logical_charge_id=NULL WHERE payment_id=$1`, p.ID); err == nil {
		t.Fatal("confirmed provider approval was rewritten")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET amount=amount+1 WHERE id=$1`, p.ID); err == nil {
		t.Fatal("posted payment amount was changed")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE ledger_entries SET amount=1 WHERE journal_id=(SELECT id FROM ledger_journals WHERE payment_id=$1)`, p.ID); err == nil {
		t.Fatal("ledger entry mutation accepted")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM ledger_journals WHERE payment_id=$1`, p.ID); err == nil {
		t.Fatal("journal deletion accepted")
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO ledger_journals
		(payment_id,purpose,posting_reference,provider_operation_id,merchant_id,currency,amount)
		VALUES ($1,'PAYMENT_APPROVAL',$2,$1,$3,'JPY',951)`, p.ID, "conflict:"+p.ID, p.MerchantID); err == nil {
		t.Fatal("conflicting second approval journal accepted")
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO ledger_entries (journal_id,account_id,side,currency,amount)
		SELECT e.journal_id,e.account_id,'DEBIT','JPY',0 FROM ledger_entries e
		JOIN ledger_journals j ON j.id=e.journal_id WHERE j.payment_id=$1 LIMIT 1`, p.ID); err == nil {
		t.Fatal("zero entry accepted")
	}
}

func TestOperatorRecoveryAndAudit(t *testing.T) {
	pool := testPool(t)
	op := operator.New(pool)
	merchant := fmt.Sprintf("operator-%d", time.Now().UnixNano())
	lost := ledgerPayment(t, pool, merchant)
	if p, err := op.Processor.Process(context.Background(), lost.ID, banking.LostApprovalResponse); err != nil || p.Status != payment.StatusReconciliationRequired {
		t.Fatalf("lost = %+v,%v", p, err)
	}
	a, err := op.Recover(context.Background(), lost.ID, "integration-test")
	if err != nil || a.State != "SUCCEEDED" || a.AfterStatus != payment.StatusCompleted {
		t.Fatalf("recover approval = %+v,%v", a, err)
	}
	if journalCount(t, pool, lost.ID) != 1 {
		t.Fatal("operator recovery missed journal")
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO payment_workflow_dispatch(payment_id,scenario,workflow_id)
		VALUES ($1,'LOST_APPROVAL_RESPONSE',$2)`, lost.ID, "payflow-payment-"+lost.ID); err != nil {
		t.Fatal(err)
	}
	inspection, err := op.Inspect(context.Background(), lost.ID)
	if err != nil || inspection.Provider == nil || inspection.Provider.Outcome != "APPROVED" ||
		inspection.Dispatch == nil || len(inspection.Journals) != 1 || len(inspection.Journals[0].Entries) != 2 {
		t.Fatalf("operator inspection = %+v,%v", inspection, err)
	}
	if _, err := op.Recover(context.Background(), lost.ID, "integration-test"); !errors.Is(err, payment.ErrInvalidTransition) {
		t.Fatalf("terminal recovery error = %v", err)
	}
	unknown := ledgerPayment(t, pool, merchant)
	if _, err := op.Processor.Process(context.Background(), unknown.ID, banking.NetworkTimeout); err != nil {
		t.Fatal(err)
	}
	a, err = op.Recover(context.Background(), unknown.ID, "integration-test")
	if err != nil || a.AfterStatus != payment.StatusReconciliationRequired || journalCount(t, pool, unknown.ID) != 0 {
		t.Fatalf("unknown recovery = %+v,%v", a, err)
	}
	var audits int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operator_actions WHERE payment_id IN ($1,$2)`, lost.ID, unknown.ID).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audit count = %d,%v", audits, err)
	}
	actions, err := op.ActionsForPayment(context.Background(), lost.ID)
	if err != nil || len(actions) != 2 || actions[0].State != "FAILED" || actions[1].State != "SUCCEEDED" {
		t.Fatalf("operator action history = %+v,%v", actions, err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM operator_actions WHERE id=$1`, actions[0].ID); err == nil {
		t.Fatal("audit record deletion accepted")
	}
}

func TestJournalFailureRollsBackPaymentCompletion(t *testing.T) {
	pool := testPool(t)
	store := &storage.Postgres{Pool: pool}
	p := ledgerPayment(t, pool, fmt.Sprintf("journal-failure-%d", time.Now().UnixNano()))
	// Inject a one-payment journal failure after the mock provider has approved.
	functionSQL := fmt.Sprintf(`CREATE FUNCTION reject_test_journal() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.payment_id = '%s'::uuid THEN RAISE EXCEPTION 'injected journal failure'; END IF;
		RETURN NEW; END; $$`, p.ID)
	if _, err := pool.Exec(context.Background(), functionSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE TRIGGER reject_test_journal
		BEFORE INSERT ON ledger_journals FOR EACH ROW EXECUTE FUNCTION reject_test_journal()`); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_test_journal ON ledger_journals`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_test_journal()`)
	}
	t.Cleanup(cleanup)
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	if _, err := processor.Process(context.Background(), p.ID, banking.Approve); err == nil {
		t.Fatal("journal failure did not abort completion")
	}
	current, err := store.Get(context.Background(), p.ID)
	if err != nil || current.Status != payment.StatusProcessing || current.ProviderOperationID == "" {
		t.Fatalf("payment after failed posting = %+v,%v", current, err)
	}
	if journalCount(t, pool, p.ID) != 0 {
		t.Fatal("partial journal survived")
	}
	if outcome, attempts, charges := providerFacts(t, pool, p.ID); outcome != "APPROVED" || attempts != 1 || charges != 1 {
		t.Fatalf("provider decision lost: %s,attempts=%d,charges=%d", outcome, attempts, charges)
	}
	cleanup()
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := processor.Reconcile(context.Background(), p.ID)
	if err != nil || recovered.Status != payment.StatusCompleted || recovered.ProviderOperationID != current.ProviderOperationID {
		t.Fatalf("recovery = %+v,%v", recovered, err)
	}
	if journalCount(t, pool, p.ID) != 1 {
		t.Fatal("recovery missed approval journal")
	}
	if _, attempts, charges := providerFacts(t, pool, p.ID); attempts != 1 || charges != 1 {
		t.Fatalf("recovery duplicated provider operation: attempts=%d,charges=%d", attempts, charges)
	}
}

func TestOperatorRecoversExpiredProcessingClaim(t *testing.T) {
	pool := testPool(t)
	op := operator.New(pool)
	p := ledgerPayment(t, pool, fmt.Sprintf("expired-operator-%d", time.Now().UnixNano()))
	claimed, ok, err := op.Store.Claim(context.Background(), p.ID, banking.Approve, p.ID, "123e4567-e89b-42d3-a456-426614174022", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %+v,%v", claimed, err)
	}
	if _, err := (&banking.Postgres{Pool: pool}).Execute(context.Background(), banking.Operation{ID: p.ID, PaymentID: p.ID, Scenario: banking.Approve}); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Recover(context.Background(), p.ID, "integration-test"); !errors.Is(err, payment.ErrInvalidTransition) {
		t.Fatalf("active claim recovery = %v", err)
	}
	if journalCount(t, pool, p.ID) != 0 {
		t.Fatal("active claim was overridden")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE payments SET processing_lease_until=now()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := op.Candidates(context.Background(), payment.StatusProcessing, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range candidates {
		if c.PaymentID == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expired claim not listed")
	}
	a, err := op.Recover(context.Background(), p.ID, "integration-test")
	if err != nil || a.State != "SUCCEEDED" || a.AfterStatus != payment.StatusCompleted || journalCount(t, pool, p.ID) != 1 {
		t.Fatalf("expired claim recovery = %+v,%v", a, err)
	}
}
