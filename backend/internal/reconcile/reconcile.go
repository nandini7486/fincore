// Package reconcile checks that FinCore's books balance (NFR-03).
//
// The ledger is the source of truth; account balances are a projection of
// it kept for fast reads. Reconciliation proves the two agree and that every
// payment was recorded correctly. In a healthy system it always reports OK;
// any finding means a bug, a manual database edit, or corruption, and must
// be investigated before more money moves.
//
// Checks (all against one database snapshot):
//  1. Every account balance equals the sum of its ledger entries
//     (credits positive, debits negative).
//  2. The whole ledger sums to zero: every rupee credited was debited
//     somewhere, so money is never created or destroyed.
//  3. Every SUCCESS payment has exactly two ledger entries: one DEBIT on the
//     sender and one CREDIT on the receiver, each for the payment's amount
//     and currency.
//  4. Payments that are not SUCCESS have no ledger entries.
package reconcile

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

// maxFindings caps how many problems of each kind are listed in a report.
// The counts are always complete.
const maxFindings = 100

// AccountMismatch is an account whose stored balance disagrees with its
// ledger history.
type AccountMismatch struct {
	AccountID     string `json:"account_id"`
	Type          string `json:"type"`
	StoredBalance int64  `json:"stored_balance_minor"`
	LedgerBalance int64  `json:"ledger_balance_minor"`
}

// PaymentProblem is a payment whose ledger entries are wrong.
type PaymentProblem struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
	Problem   string `json:"problem"`
}

type Report struct {
	AccountsChecked int64 `json:"accounts_checked"`
	PaymentsChecked int64 `json:"payments_checked"`
	LedgerEntries   int64 `json:"ledger_entries"`
	// LedgerSum must be 0.
	LedgerSum int64 `json:"ledger_sum_minor"`

	AccountMismatchCount int64             `json:"account_mismatch_count"`
	AccountMismatches    []AccountMismatch `json:"account_mismatches"`
	PaymentProblemCount  int64             `json:"payment_problem_count"`
	PaymentProblems      []PaymentProblem  `json:"payment_problems"`
}

// OK reports whether the books balance.
func (r Report) OK() bool {
	return r.LedgerSum == 0 && r.AccountMismatchCount == 0 && r.PaymentProblemCount == 0
}

// Run reconciles inside a REPEATABLE READ snapshot, so payments committed
// while it runs cannot produce false findings, and writes a
// reconciliation.run audit record in the same transaction (Blueprint §8.3).
func Run(ctx context.Context, b db.SnapshotBeginner) (Report, error) {
	var rep Report
	err := db.WithSnapshotTx(ctx, b, func(tx pgx.Tx) error {
		var err error
		if rep, err = check(ctx, tx); err != nil {
			return err
		}
		outcome := audit.Success
		if !rep.OK() {
			outcome = audit.Failure
		}
		return audit.Write(ctx, tx, audit.Entry{
			Action:       "reconciliation.run",
			ResourceType: "ledger",
			Outcome:      outcome,
			Details: map[string]any{
				"accounts_checked":       rep.AccountsChecked,
				"payments_checked":       rep.PaymentsChecked,
				"ledger_entries":         rep.LedgerEntries,
				"ledger_sum_minor":       rep.LedgerSum,
				"account_mismatch_count": rep.AccountMismatchCount,
				"payment_problem_count":  rep.PaymentProblemCount,
			},
		})
	})
	return rep, err
}

func check(ctx context.Context, tx pgx.Tx) (Report, error) {
	rep := Report{AccountMismatches: []AccountMismatch{}, PaymentProblems: []PaymentProblem{}}

	err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM accounts),
		       (SELECT count(*) FROM payments),
		       (SELECT count(*) FROM ledger_entries),
		       (SELECT coalesce(sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE -amount END), 0)
		        FROM ledger_entries)`,
	).Scan(&rep.AccountsChecked, &rep.PaymentsChecked, &rep.LedgerEntries, &rep.LedgerSum)
	if err != nil {
		return Report{}, fmt.Errorf("reconcile totals: %w", err)
	}

	// 1. Balances against ledger history.
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.type, a.balance, coalesce(l.net, 0), count(*) OVER ()
		FROM accounts a
		LEFT JOIN (
			SELECT account_id,
			       sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE -amount END) AS net
			FROM ledger_entries
			GROUP BY account_id
		) l ON l.account_id = a.id
		WHERE a.balance <> coalesce(l.net, 0)
		ORDER BY a.id
		LIMIT $1`, maxFindings)
	if err != nil {
		return Report{}, fmt.Errorf("reconcile balances: %w", err)
	}
	for rows.Next() {
		var m AccountMismatch
		if err := rows.Scan(&m.AccountID, &m.Type, &m.StoredBalance, &m.LedgerBalance, &rep.AccountMismatchCount); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("scan balance mismatch: %w", err)
		}
		rep.AccountMismatches = append(rep.AccountMismatches, m)
	}
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("reconcile balances: %w", err)
	}

	// 3 and 4. Each payment's ledger entries.
	rows, err = tx.Query(ctx, `
		SELECT p.id, p.status, e.good_debits, e.good_credits, e.total, count(*) OVER ()
		FROM payments p
		CROSS JOIN LATERAL (
			SELECT count(*) FILTER (WHERE entry_type = 'DEBIT'  AND account_id = p.sender_account_id
			                          AND amount = p.amount AND currency = p.currency) AS good_debits,
			       count(*) FILTER (WHERE entry_type = 'CREDIT' AND account_id = p.receiver_account_id
			                          AND amount = p.amount AND currency = p.currency) AS good_credits,
			       count(*) AS total
			FROM ledger_entries
			WHERE payment_id = p.id
		) e
		WHERE (p.status = 'SUCCESS' AND NOT (e.good_debits = 1 AND e.good_credits = 1 AND e.total = 2))
		   OR (p.status <> 'SUCCESS' AND e.total <> 0)
		ORDER BY p.id
		LIMIT $1`, maxFindings)
	if err != nil {
		return Report{}, fmt.Errorf("reconcile payments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p PaymentProblem
		var goodDebits, goodCredits, total int64
		if err := rows.Scan(&p.PaymentID, &p.Status, &goodDebits, &goodCredits, &total, &rep.PaymentProblemCount); err != nil {
			return Report{}, fmt.Errorf("scan payment problem: %w", err)
		}
		if p.Status == "SUCCESS" {
			p.Problem = fmt.Sprintf(
				"expected 1 matching DEBIT and 1 matching CREDIT (2 entries), found %d matching debits, %d matching credits, %d entries",
				goodDebits, goodCredits, total)
		} else {
			p.Problem = fmt.Sprintf("%s payment has %d ledger entries, expected 0", p.Status, total)
		}
		rep.PaymentProblems = append(rep.PaymentProblems, p)
	}
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("reconcile payments: %w", err)
	}
	return rep, nil
}
