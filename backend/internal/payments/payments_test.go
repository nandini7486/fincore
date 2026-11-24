package payments_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

type fixture struct {
	t        *testing.T
	db       *testdb.DB
	admin    string
	treasury string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, db: testdb.New(t)}
	f.admin = f.user("admin@example.com", auth.RoleAdmin)
	if err := f.db.Owner.QueryRow(context.Background(),
		`SELECT id FROM accounts WHERE type = 'TREASURY'`).Scan(&f.treasury); err != nil {
		t.Fatal(err)
	}
	// Every scenario must leave the books balanced.
	t.Cleanup(f.assertInvariants)
	return f
}

func (f *fixture) user(email string, role auth.Role) string {
	f.t.Helper()
	var id auth.Identity
	err := db.WithTx(context.Background(), f.db.Owner, func(tx pgx.Tx) error {
		var err error
		id, _, err = users.Create(context.Background(), tx, users.NewUser{Name: "Test", Email: email, Role: role})
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id.UserID
}

func (f *fixture) account(owner string) string {
	f.t.Helper()
	var a accounts.Account
	err := db.WithTx(context.Background(), f.db.App, func(tx pgx.Tx) error {
		var err error
		a, err = accounts.Create(context.Background(), tx, owner, "")
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return a.ID
}

// run executes a payment the way the API does: as fincore_app, committing
// business-rule failures and rolling back everything else.
func (f *fixture) run(fn func(context.Context, pgx.Tx, payments.Request) (payments.Payment, error), r payments.Request) (payments.Payment, error) {
	var p payments.Payment
	var ruleErr *payments.RuleError
	err := db.WithTx(context.Background(), f.db.App, func(tx pgx.Tx) error {
		var err error
		p, err = fn(context.Background(), tx, r)
		if errors.As(err, &ruleErr) {
			return nil
		}
		return err
	})
	if err == nil && ruleErr != nil {
		return ruleErr.Payment, ruleErr
	}
	return p, err
}

func (f *fixture) topUp(account string, amount int64) payments.Payment {
	f.t.Helper()
	p, err := f.run(payments.CreateTopUp, payments.Request{
		ReceiverID: account, Amount: amount, Currency: "INR", InitiatedBy: f.admin})
	if err != nil {
		f.t.Fatalf("top-up: %v", err)
	}
	return p
}

func (f *fixture) transfer(by, from, to string, amount int64) (payments.Payment, error) {
	return f.run(payments.CreateTransfer, payments.Request{
		SenderID: from, ReceiverID: to, Amount: amount, Currency: "INR", InitiatedBy: by})
}

func (f *fixture) balance(account string) int64 {
	f.t.Helper()
	var b int64
	if err := f.db.Owner.QueryRow(context.Background(),
		`SELECT balance FROM accounts WHERE id = $1`, account).Scan(&b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// assertInvariants checks NFR-03: every balance equals its ledger history,
// and the whole ledger sums to zero (credits positive, debits negative).
func (f *fixture) assertInvariants() {
	f.t.Helper()
	ctx := context.Background()
	drift := f.count(`
		SELECT count(*) FROM accounts a
		LEFT JOIN (
			SELECT account_id,
			       sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE -amount END) AS net
			FROM ledger_entries GROUP BY account_id
		) l ON l.account_id = a.id
		WHERE a.balance <> coalesce(l.net, 0)`)
	if drift != 0 {
		f.t.Errorf("invariant: %d account balance(s) disagree with the ledger", drift)
	}
	var total int64
	if err := f.db.Owner.QueryRow(ctx, `
		SELECT coalesce(sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE -amount END), 0)
		FROM ledger_entries`).Scan(&total); err != nil {
		f.t.Fatal(err)
	}
	if total != 0 {
		f.t.Errorf("invariant: global ledger sum = %d, want 0", total)
	}
	unbalanced := f.count(`
		SELECT count(*) FROM payments p
		WHERE p.status = 'SUCCESS' AND (
			SELECT count(*) FROM ledger_entries e WHERE e.payment_id = p.id) <> 2`)
	if unbalanced != 0 {
		f.t.Errorf("invariant: %d successful payment(s) without exactly one debit and one credit", unbalanced)
	}
	if n := f.count(`SELECT count(*) FROM ledger_entries e JOIN payments p ON p.id = e.payment_id WHERE p.status <> 'SUCCESS'`); n != 0 {
		f.t.Errorf("invariant: %d ledger entries belong to non-successful payments", n)
	}
}

func TestTopUpAndTransfer(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)

	top := f.topUp(aliceAcct, 500000) // ₹5,000
	if top.Type != payments.TopUp || top.Status != "SUCCESS" || top.SenderID != f.treasury {
		t.Fatalf("top-up = %+v", top)
	}
	if got := f.balance(f.treasury); got != -500000 {
		t.Fatalf("treasury = %d, want -500000 (money issued)", got)
	}

	p, err := f.transfer(alice, aliceAcct, bobAcct, 200000) // ₹2,000
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != payments.Transfer || p.Status != "SUCCESS" || p.FailureReason != nil || p.ID == "" {
		t.Fatalf("payment = %+v", p)
	}
	if a, b := f.balance(aliceAcct), f.balance(bobAcct); a != 300000 || b != 200000 {
		t.Fatalf("balances alice=%d bob=%d, want 300000 and 200000", a, b)
	}

	// One DEBIT on the sender and one CREDIT on the receiver, same amount.
	if n := f.count(`
		SELECT count(*) FROM ledger_entries
		WHERE payment_id = $1 AND amount = 200000 AND (
			(entry_type = 'DEBIT' AND account_id = $2) OR (entry_type = 'CREDIT' AND account_id = $3))`,
		p.ID, aliceAcct, bobAcct); n != 2 {
		t.Fatalf("ledger entries = %d, want 2", n)
	}

	// Both money movements are audited with their actors.
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'topup.create' AND outcome = 'SUCCESS' AND actor_user_id = $1 AND resource_id = $2`, f.admin, top.ID); n != 1 {
		t.Fatalf("topup.create audit = %d, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'payment.create' AND outcome = 'SUCCESS' AND actor_user_id = $1 AND resource_id = $2`, alice, p.ID); n != 1 {
		t.Fatalf("payment.create audit = %d, want 1", n)
	}
}

func TestBusinessRuleFailuresAreStoredWithoutMovingMoney(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 10000) // ₹100

	t.Run("insufficient funds", func(t *testing.T) {
		p, err := f.transfer(alice, aliceAcct, bobAcct, 10001)
		var ruleErr *payments.RuleError
		if !errors.As(err, &ruleErr) || *ruleErr.Payment.FailureReason != payments.ReasonInsufficientFunds {
			t.Fatalf("err = %v, want INSUFFICIENT_FUNDS", err)
		}
		if p.Status != "FAILED" || p.ID == "" {
			t.Fatalf("payment = %+v", p)
		}
		if n := f.count(`SELECT count(*) FROM payments WHERE id = $1 AND status = 'FAILED' AND failure_reason = 'INSUFFICIENT_FUNDS'`, p.ID); n != 1 {
			t.Fatal("FAILED payment was not stored")
		}
		if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'payment.create' AND outcome = 'FAILURE' AND resource_id = $1`, p.ID); n != 1 {
			t.Fatal("failure was not audited")
		}
	})

	t.Run("exact balance succeeds", func(t *testing.T) {
		if _, err := f.transfer(alice, aliceAcct, bobAcct, 10000); err != nil {
			t.Fatal(err)
		}
		if got := f.balance(aliceAcct); got != 0 {
			t.Fatalf("alice = %d, want 0", got)
		}
	})

	t.Run("inactive receiver", func(t *testing.T) {
		if _, err := f.db.Owner.Exec(context.Background(), `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, aliceAcct); err != nil {
			t.Fatal(err)
		}
		_, err := f.transfer(bob, bobAcct, aliceAcct, 100)
		var ruleErr *payments.RuleError
		if !errors.As(err, &ruleErr) || *ruleErr.Payment.FailureReason != payments.ReasonAccountInactive {
			t.Fatalf("err = %v, want ACCOUNT_INACTIVE", err)
		}
	})

	if a, b := f.balance(aliceAcct), f.balance(bobAcct); a != 0 || b != 10000 {
		t.Fatalf("balances alice=%d bob=%d after failures, want 0 and 10000", a, b)
	}
}

func TestOnlyValidAccountsCanTakePart(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 10000)
	missing := "00000000-0000-4000-8000-000000000000"

	for _, tc := range []struct {
		name         string
		by, from, to string
		want         error
	}{
		{"sender owned by someone else", bob, aliceAcct, bobAcct, payments.ErrSenderNotFound},
		{"treasury as transfer sender", alice, f.treasury, bobAcct, payments.ErrSenderNotFound},
		{"missing sender", alice, missing, bobAcct, payments.ErrSenderNotFound},
		{"missing receiver", alice, aliceAcct, missing, payments.ErrReceiverNotFound},
		{"treasury as receiver", alice, aliceAcct, f.treasury, payments.ErrReceiverNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.transfer(tc.by, tc.from, tc.to, 100); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	_, err := f.run(payments.CreateTopUp, payments.Request{ReceiverID: f.treasury, Amount: 100, Currency: "INR", InitiatedBy: f.admin})
	if !errors.Is(err, payments.ErrReceiverNotFound) {
		t.Fatalf("top-up of treasury: err = %v, want ErrReceiverNotFound", err)
	}

	// None of these are payments, so nothing was stored.
	if n := f.count(`SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 0 {
		t.Fatalf("transfers stored = %d, want 0", n)
	}
}

// Many simultaneous payments from one account must never overdraw it: the
// balance check runs while the row is locked (NFR-02).
func TestConcurrentTransfersNeverOverdraw(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 1000) // ₹10

	const attempts = 25 // each ₹1, so exactly 10 can succeed
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, insufficient := 0, 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.transfer(alice, aliceAcct, bobAcct, 100)
			var ruleErr *payments.RuleError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.As(err, &ruleErr) && *ruleErr.Payment.FailureReason == payments.ReasonInsufficientFunds:
				insufficient++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != 10 || insufficient != attempts-10 {
		t.Fatalf("succeeded=%d insufficient=%d, want 10 and %d", succeeded, insufficient, attempts-10)
	}
	if a, b := f.balance(aliceAcct), f.balance(bobAcct); a != 0 || b != 1000 {
		t.Fatalf("balances alice=%d bob=%d, want 0 and 1000", a, b)
	}
}

// A->B and B->A at the same time would deadlock if each locked its own
// sender first. Locking both rows in ID order prevents that.
func TestOppositeTransfersDoNotDeadlock(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 100000)
	f.topUp(bobAcct, 100000)

	const rounds = 30
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := f.transfer(alice, aliceAcct, bobAcct, 100); err != nil {
				t.Errorf("alice->bob: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.transfer(bob, bobAcct, aliceAcct, 100); err != nil {
				t.Errorf("bob->alice: %v", err)
			}
		}()
	}
	wg.Wait()

	if a, b := f.balance(aliceAcct), f.balance(bobAcct); a != 100000 || b != 100000 {
		t.Fatalf("balances alice=%d bob=%d, want both 100000", a, b)
	}
}

// If another transaction holds the account lock too long, the payment gives
// up with ErrBusy (API: 503, safe to retry) instead of waiting forever.
func TestLockTimeoutReturnsBusy(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 1000)

	blocker, err := f.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, aliceAcct); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = f.transfer(alice, aliceAcct, bobAcct, 100)
	if !errors.Is(err, payments.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited > 4*time.Second {
		t.Fatalf("waited %v; lock_timeout should stop it after about 2s", waited)
	}
	_ = blocker.Rollback(ctx)

	// Nothing from the timed-out attempt was committed.
	if n := f.count(`SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 0 {
		t.Fatalf("payments stored after timeout = %d, want 0", n)
	}
	if got := f.balance(aliceAcct); got != 1000 {
		t.Fatalf("alice = %d, want 1000", got)
	}
}
