package reconcile_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/reconcile"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

// books is a database with real activity: two funded users and a few
// transfers, including a FAILED one.
type books struct {
	t                     *testing.T
	db                    *testdb.DB
	admin, alice, bob     string
	aliceAcct, bobAcct    string
	carolAcct, failedID   string
	successID, successAmt string
}

func newBooks(t *testing.T) *books {
	t.Helper()
	b := &books{t: t, db: testdb.New(t)}
	b.admin = b.user("admin@example.com", auth.RoleAdmin)
	b.alice = b.user("alice@example.com", auth.RoleUser)
	b.bob = b.user("bob@example.com", auth.RoleUser)
	b.aliceAcct, b.bobAcct = b.account(b.alice), b.account(b.bob)
	b.carolAcct = b.account(b.user("carol@example.com", auth.RoleUser))

	b.move(payments.CreateTopUp, payments.Request{ReceiverID: b.aliceAcct, Amount: 10000, Currency: "INR", InitiatedBy: b.admin})
	b.successID = b.move(payments.CreateTransfer, payments.Request{SenderID: b.aliceAcct, ReceiverID: b.bobAcct, Amount: 2500, Currency: "INR", InitiatedBy: b.alice})
	b.failedID = b.move(payments.CreateTransfer, payments.Request{SenderID: b.bobAcct, ReceiverID: b.aliceAcct, Amount: 999999, Currency: "INR", InitiatedBy: b.bob})
	return b
}

func (b *books) user(email string, role auth.Role) string {
	b.t.Helper()
	var id auth.Identity
	err := db.WithTx(context.Background(), b.db.Owner, func(tx pgx.Tx) error {
		var err error
		id, _, err = users.Create(context.Background(), tx, users.NewUser{Name: "T", Email: email, Role: role})
		return err
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return id.UserID
}

func (b *books) account(owner string) string {
	b.t.Helper()
	var a accounts.Account
	err := db.WithTx(context.Background(), b.db.App, func(tx pgx.Tx) error {
		var err error
		a, err = accounts.Create(context.Background(), tx, owner, "")
		return err
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return a.ID
}

// move runs a payment like the API does and returns its ID.
func (b *books) move(fn func(context.Context, pgx.Tx, payments.Request) (payments.Payment, error), r payments.Request) string {
	b.t.Helper()
	var p payments.Payment
	var ruleErr *payments.RuleError
	err := db.WithTx(context.Background(), b.db.App, func(tx pgx.Tx) error {
		var err error
		p, err = fn(context.Background(), tx, r)
		if errors.As(err, &ruleErr) {
			p = ruleErr.Payment
			return nil
		}
		return err
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return p.ID
}

func (b *books) exec(sql string, args ...any) {
	b.t.Helper()
	if _, err := b.db.Owner.Exec(context.Background(), sql, args...); err != nil {
		b.t.Fatalf("%s: %v", sql, err)
	}
}

// fakeSuccess inserts a SUCCESS payment directly, bypassing the payments
// package, so a test can then give it wrong ledger entries.
func (b *books) fakeSuccess(from, to string, amount int64) string {
	b.t.Helper()
	var id string
	err := b.db.Owner.QueryRow(context.Background(), `
		INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
		                      status, initiated_by_user_id, request_id)
		VALUES ('TRANSFER', $1, $2, $3, 'INR', 'SUCCESS', $4, 'req_corrupt')
		RETURNING id`, from, to, amount, b.alice).Scan(&id)
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

func (b *books) run() reconcile.Report {
	b.t.Helper()
	rep, err := reconcile.Run(context.Background(), b.db.App)
	if err != nil {
		b.t.Fatal(err)
	}
	return rep
}

func TestHealthyBooksReconcile(t *testing.T) {
	b := newBooks(t)
	rep := b.run()

	if !rep.OK() {
		t.Fatalf("healthy books reported problems: %+v", rep)
	}
	// 4 accounts incl. treasury; top-up + transfer + failed transfer.
	if rep.AccountsChecked != 4 || rep.PaymentsChecked != 3 || rep.LedgerEntries != 4 || rep.LedgerSum != 0 {
		t.Fatalf("report = %+v", rep)
	}

	var outcome string
	if err := b.db.Owner.QueryRow(context.Background(),
		`SELECT outcome FROM audit_log WHERE action = 'reconciliation.run'`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "SUCCESS" {
		t.Fatalf("audit outcome = %s, want SUCCESS", outcome)
	}
}

// Each case corrupts the books the way a bug or a manual edit could, and
// asserts exactly which checks catch it.
func TestReconcileDetectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name             string
		corrupt          func(b *books)
		wantAccountDrift int64
		wantPaymentProbs int64
		wantSumNonZero   bool
		wantProblemText  string
	}{
		{
			name: "balance edited directly",
			corrupt: func(b *books) {
				b.exec(`UPDATE accounts SET balance = balance + 1 WHERE id = $1`, b.bobAcct)
			},
			wantAccountDrift: 1,
		},
		{
			name: "ledger entry recorded for a FAILED payment",
			corrupt: func(b *books) {
				b.exec(`INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
				        VALUES ($1, $2, 'CREDIT', 999999, 'INR')`, b.failedID, b.aliceAcct)
			},
			wantAccountDrift: 1, wantPaymentProbs: 1, wantSumNonZero: true,
			wantProblemText: "FAILED payment has 1 ledger entries, expected 0",
		},
		{
			name: "credit smaller than debit, balances made to match the ledger",
			corrupt: func(b *books) {
				id := b.fakeSuccess(b.aliceAcct, b.bobAcct, 100)
				b.exec(`INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
				        VALUES ($1, $2, 'DEBIT', 100, 'INR'), ($1, $3, 'CREDIT', 90, 'INR')`, id, b.aliceAcct, b.bobAcct)
				b.exec(`UPDATE accounts SET balance = balance - 100 WHERE id = $1`, b.aliceAcct)
				b.exec(`UPDATE accounts SET balance = balance + 90 WHERE id = $1`, b.bobAcct)
			},
			wantPaymentProbs: 1, wantSumNonZero: true,
			wantProblemText: "found 1 matching debits, 0 matching credits",
		},
		{
			name: "successful payment missing its credit",
			corrupt: func(b *books) {
				id := b.fakeSuccess(b.aliceAcct, b.bobAcct, 100)
				b.exec(`INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
				        VALUES ($1, $2, 'DEBIT', 100, 'INR')`, id, b.aliceAcct)
				b.exec(`UPDATE accounts SET balance = balance - 100 WHERE id = $1`, b.aliceAcct)
			},
			wantPaymentProbs: 1, wantSumNonZero: true,
			wantProblemText: "1 entries",
		},
		{
			// The books still balance in total and per account. Only the
			// per-payment check notices the money went to the wrong person.
			name: "credit posted to the wrong account",
			corrupt: func(b *books) {
				id := b.fakeSuccess(b.aliceAcct, b.bobAcct, 100)
				b.exec(`INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
				        VALUES ($1, $2, 'DEBIT', 100, 'INR'), ($1, $3, 'CREDIT', 100, 'INR')`, id, b.aliceAcct, b.carolAcct)
				b.exec(`UPDATE accounts SET balance = balance - 100 WHERE id = $1`, b.aliceAcct)
				b.exec(`UPDATE accounts SET balance = balance + 100 WHERE id = $1`, b.carolAcct)
			},
			wantPaymentProbs: 1,
			wantProblemText:  "0 matching credits",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBooks(t)
			if !b.run().OK() {
				t.Fatal("books were not healthy before corruption")
			}
			tc.corrupt(b)
			rep := b.run()

			if rep.OK() {
				t.Fatalf("corruption not detected: %+v", rep)
			}
			if rep.AccountMismatchCount != tc.wantAccountDrift {
				t.Errorf("account mismatches = %d, want %d (%+v)", rep.AccountMismatchCount, tc.wantAccountDrift, rep.AccountMismatches)
			}
			if rep.PaymentProblemCount != tc.wantPaymentProbs {
				t.Errorf("payment problems = %d, want %d (%+v)", rep.PaymentProblemCount, tc.wantPaymentProbs, rep.PaymentProblems)
			}
			if (rep.LedgerSum != 0) != tc.wantSumNonZero {
				t.Errorf("ledger sum = %d, want non-zero=%v", rep.LedgerSum, tc.wantSumNonZero)
			}
			if tc.wantProblemText != "" && (len(rep.PaymentProblems) == 0 || !strings.Contains(rep.PaymentProblems[0].Problem, tc.wantProblemText)) {
				t.Errorf("problem text = %+v, want it to contain %q", rep.PaymentProblems, tc.wantProblemText)
			}

			var n int
			if err := b.db.Owner.QueryRow(context.Background(),
				`SELECT count(*) FROM audit_log WHERE action = 'reconciliation.run' AND outcome = 'FAILURE'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("FAILURE audit records = %d, want 1", n)
			}
		})
	}
}

// Reconciliation running while payments commit must never report a false
// problem: it reads one consistent snapshot.
func TestReconcileDuringConcurrentPayments(t *testing.T) {
	b := newBooks(t)
	b.move(payments.CreateTopUp, payments.Request{ReceiverID: b.bobAcct, Amount: 100000, Currency: "INR", InitiatedBy: b.admin})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, pair := range [][3]string{{b.alice, b.aliceAcct, b.bobAcct}, {b.bob, b.bobAcct, b.aliceAcct}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.move(payments.CreateTransfer, payments.Request{SenderID: pair[1], ReceiverID: pair[2], Amount: 1, Currency: "INR", InitiatedBy: pair[0]})
			}
		}()
	}
	for i := 0; i < 25; i++ {
		if rep := b.run(); !rep.OK() {
			close(stop)
			wg.Wait()
			t.Fatalf("run %d reported a false problem during concurrent payments: %+v", i, rep)
		}
	}
	close(stop)
	wg.Wait()
	if rep := b.run(); !rep.OK() || rep.PaymentsChecked < 10 {
		t.Fatalf("final report = %+v", rep)
	}
}
