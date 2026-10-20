// Package verification checks financial invariants after a local benchmark.
// It never repairs data or infers an unknown provider outcome.
package verification

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Report struct {
	Payments        int      `json:"payments"`
	Completed       int      `json:"completed"`
	Declined        int      `json:"declined"`
	Unresolved      int      `json:"unresolved"`
	Journals        int      `json:"approval_journals"`
	ExpectedPayable int64    `json:"expected_merchant_payable"`
	ActualPayable   int64    `json:"actual_merchant_payable"`
	ViolationCount  int      `json:"violation_count"`
	Violations      []string `json:"violations,omitempty"`
	Passed          bool     `json:"passed"`
}

func (r *Report) add(paymentID, reason string) {
	r.ViolationCount++
	if len(r.Violations) < 100 {
		r.Violations = append(r.Violations, paymentID+": "+reason)
	}
}

// Check validates every payment and journal for one synthetic merchant. The
// database constraints also enforce uniqueness and immutability at write time;
// this independent read checks what was actually persisted after the run.
func Check(ctx context.Context, pool *pgxpool.Pool, merchant string) (Report, error) {
	var report Report
	rows, err := pool.Query(ctx, `SELECT p.id::text,p.status,p.amount,p.currency,
		COALESCE(p.provider_operation_id::text,''),COALESCE(o.operation_id::text,''),
		COALESCE(o.outcome,''),COALESCE(o.logical_charge_id::text,''),
		count(DISTINCT j.id),count(e.id),
		COALESCE(sum(CASE WHEN e.side='DEBIT' THEN e.amount ELSE 0 END),0),
		COALESCE(sum(CASE WHEN e.side='CREDIT' THEN e.amount ELSE 0 END),0),
		COALESCE(max(j.amount),0),COALESCE(max(j.currency),''),
		COALESCE(max(j.merchant_id),''),COALESCE(max(j.provider_operation_id::text),''),
		COALESCE(max(j.posting_reference),''),
		COALESCE(bool_and(e.id IS NULL OR
			(e.side='DEBIT' AND a.kind='PROVIDER_RECEIVABLE' AND a.merchant_id IS NULL)
			OR (e.side='CREDIT' AND a.kind='MERCHANT_PAYABLE' AND a.merchant_id=p.merchant_id)),true),
		COALESCE(bool_and(e.id IS NULL OR e.currency=p.currency),true)
		FROM payments p
		LEFT JOIN simulated_provider_operations o ON o.payment_id=p.id
		LEFT JOIN ledger_journals j ON j.payment_id=p.id AND j.purpose='PAYMENT_APPROVAL'
		LEFT JOIN ledger_entries e ON e.journal_id=j.id
		LEFT JOIN ledger_accounts a ON a.id=e.account_id
		WHERE p.merchant_id=$1
		GROUP BY p.id,o.operation_id,o.outcome,o.logical_charge_id`, merchant)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var id, status, currency, operationID, providerID, outcome, chargeID string
		var journalCurrency, journalMerchant, journalOperation, reference string
		var amount, debit, credit, journalAmount int64
		var journals, entries int
		var rolesValid, currenciesValid bool
		if err := rows.Scan(&id, &status, &amount, &currency, &operationID, &providerID, &outcome, &chargeID,
			&journals, &entries, &debit, &credit, &journalAmount, &journalCurrency, &journalMerchant,
			&journalOperation, &reference, &rolesValid, &currenciesValid); err != nil {
			rows.Close()
			return report, err
		}
		report.Payments++
		report.Journals += journals
		if operationID != "" && providerID != "" && operationID != providerID {
			report.add(id, "provider operation ID differs from payment")
		}
		if journals > 0 && (entries != 2*journals || debit != amount || credit != amount || journalAmount != amount ||
			journalCurrency != currency || journalMerchant != merchant || journalOperation != operationID ||
			reference != "approval:"+id || !rolesValid || !currenciesValid) {
			report.add(id, "approval journal entries or metadata are invalid")
		}
		switch status {
		case "COMPLETED":
			report.Completed++
			report.ExpectedPayable += amount
			if journals != 1 || operationID == "" || providerID != operationID || outcome != "APPROVED" || chargeID != operationID {
				report.add(id, "completion lacks exactly one matching provider approval and journal")
			}
		case "FAILED":
			report.Declined++
			if journals != 0 || outcome != "DECLINED" || providerID != operationID || chargeID != "" {
				report.add(id, "decline has an approval journal or lacks a provider decline")
			}
		case "CREATED", "PROCESSING", "RECONCILIATION_REQUIRED":
			report.Unresolved++
			if journals != 0 {
				report.add(id, "nonterminal payment has an approval journal")
			}
		default:
			report.add(id, "unknown payment status")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN e.side='CREDIT' THEN e.amount ELSE -e.amount END),0)
		FROM ledger_entries e JOIN ledger_accounts a ON a.id=e.account_id
		WHERE a.kind='MERCHANT_PAYABLE' AND a.merchant_id=$1 AND a.currency='JPY'`, merchant).Scan(&report.ActualPayable); err != nil {
		return report, fmt.Errorf("merchant balance: %w", err)
	}
	if report.ActualPayable != report.ExpectedPayable {
		report.add("merchant", "payable balance differs from completed payment total")
	}
	var duplicates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT j.posting_reference FROM ledger_journals j
		JOIN payments p ON p.id=j.payment_id WHERE p.merchant_id=$1
		GROUP BY j.posting_reference HAVING count(*)>1) duplicate_references`, merchant).Scan(&duplicates); err != nil {
		return report, err
	}
	if duplicates != 0 {
		report.add("merchant", "duplicate posting references")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT o.payment_id FROM simulated_provider_operations o
		JOIN payments p ON p.id=o.payment_id WHERE p.merchant_id=$1
		GROUP BY o.payment_id HAVING count(*)>1) duplicate_provider_operations`, merchant).Scan(&duplicates); err != nil {
		return report, err
	}
	if duplicates != 0 {
		report.add("merchant", "multiple provider operations for one payment")
	}
	report.Passed = report.ViolationCount == 0
	return report, nil
}
