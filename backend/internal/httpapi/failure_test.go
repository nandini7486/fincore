package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/reconcile"
)

// assertNoTrace checks that a failed payment attempt left nothing behind:
// no transfer, no ledger entries for it, no idempotency record, no
// payment.create audit, alice's balance untouched, and the books balance.
func (m *moneyEnv) assertNoTrace(t *testing.T, key, wantAliceBalance string) {
	t.Helper()
	if n := m.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 0 {
		t.Errorf("transfers after failure = %d, want 0", n)
	}
	if n := m.count(t, `SELECT count(*) FROM ledger_entries e JOIN payments p ON p.id = e.payment_id WHERE p.type = 'TRANSFER'`); n != 0 {
		t.Errorf("transfer ledger entries after failure = %d, want 0", n)
	}
	if n := m.count(t, `SELECT count(*) FROM idempotency_records WHERE key = $1`, key); n != 0 {
		t.Errorf("idempotency record survived the failure: the key would be burned")
	}
	if n := m.count(t, `SELECT count(*) FROM audit_log WHERE action = 'payment.create'`); n != 0 {
		t.Errorf("payment.create audit records after failure = %d, want 0", n)
	}
	if n := m.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'payment.succeeded'`); n != 0 {
		t.Errorf("payment.succeeded events after failure = %d, want 0 (an event for money that never moved)", n)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != wantAliceBalance {
		t.Errorf("alice balance = %s, want %s (unchanged)", got, wantAliceBalance)
	}
	rep, err := reconcile.Run(context.Background(), m.db.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("books do not balance after the failure: %+v", rep)
	}
}

// retrySucceeds sends the same request again and expects a fresh 201: the
// failed attempt must not have consumed the Idempotency-Key.
func (m *moneyEnv) retrySucceeds(t *testing.T, key, body, wantAliceBalance string) {
	t.Helper()
	r := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
	if r.code != http.StatusCreated || r.replayed {
		t.Fatalf("retry = %d replayed=%v, want a fresh 201", r.code, r.replayed)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != wantAliceBalance {
		t.Fatalf("alice balance after retry = %s, want %s", got, wantAliceBalance)
	}
}

func (m *moneyEnv) owner(t *testing.T, sql string) {
	t.Helper()
	if _, err := m.db.Owner.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// If any write inside the payment transaction fails, nothing may be
// committed: not the payment, not half the ledger, not the key (NFR-03).
// The failure is injected by revoking one privilege from the app's role.
func TestWriteFailureInsidePaymentRollsBackEverything(t *testing.T) {
	for _, tc := range []struct{ name, table string }{
		{"audit insert fails", "audit_log"},
		{"ledger insert fails", "ledger_entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fundedAlice(t)
			key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

			m.owner(t, "REVOKE INSERT ON "+tc.table+" FROM fincore_app")
			failed := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
			m.owner(t, "GRANT INSERT ON "+tc.table+" TO fincore_app")

			if failed.code != http.StatusInternalServerError || failed.errCode != "INTERNAL" {
				t.Fatalf("got %d %s, want 500 INTERNAL", failed.code, failed.errCode)
			}
			m.assertNoTrace(t, key, "100.00")
			m.retrySucceeds(t, key, body, "90.00")
		})
	}
}

// Failures are isolated: a broken ledger cannot stop a business-rule
// failure from being recorded, because a FAILED payment writes no ledger.
func TestRuleFailureDoesNotNeedTheLedger(t *testing.T) {
	m := fundedAlice(t)
	m.owner(t, "REVOKE INSERT ON ledger_entries FROM fincore_app")
	defer m.owner(t, "GRANT INSERT ON ledger_entries TO fincore_app")

	r := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "500.00")
	if r.code != http.StatusUnprocessableEntity || r.errCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("got %d %s, want 422 INSUFFICIENT_FUNDS", r.code, r.errCode)
	}
}

// waitingBackend returns the PID of the server-side connection that is
// blocked waiting for a row lock (our payment), polling until it appears.
func (m *moneyEnv) waitingBackend(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond) // well inside lock_timeout (2s)
	for time.Now().Before(deadline) {
		var pid int
		err := m.db.Owner.QueryRow(context.Background(), `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%FOR UPDATE%' AND pid <> pg_backend_pid()
			LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		if err != pgx.ErrNoRows {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("payment never reached the account lock")
	return 0
}

// holdAccountLock locks alice's account row from another session, so a
// payment request stops mid-transaction (after claiming its key and writing
// nothing else yet) and waits.
func (m *moneyEnv) holdAccountLock(t *testing.T) (release func()) {
	t.Helper()
	ctx := context.Background()
	blocker, err := m.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, m.aliceAcct); err != nil {
		t.Fatal(err)
	}
	return func() { _ = blocker.Rollback(ctx) }
}

// The database connection dies in the middle of the payment transaction
// (network failure, database restart, failover). PostgreSQL rolls back the
// open transaction; the client gets a 500 and may safely retry.
func TestDatabaseConnectionLostMidTransaction(t *testing.T) {
	m := fundedAlice(t)
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	release := m.holdAccountLock(t)
	done := make(chan *httpResult, 1)
	go func() { done <- result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body)) }()

	pid := m.waitingBackend(t)
	var killed bool
	if err := m.db.Owner.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate backend %d: %v %v", pid, killed, err)
	}
	r := <-done
	release()

	if r.code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", r.code, r.errCode)
	}
	m.assertNoTrace(t, key, "100.00")
	// The pool replaces the dead connection; the retry goes through.
	m.retrySucceeds(t, key, body, "90.00")
}

// The client gives up while the payment is still in progress (mobile app
// closed, request timeout). The request context is cancelled, the
// transaction rolls back, and the server records a 499 rather than an
// internal error. The client's retry with the same key then charges once.
func TestClientDisconnectsMidPaymentThenRetries(t *testing.T) {
	m := fundedAlice(t)
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	release := m.holdAccountLock(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+m.aliceToken)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { m.handler.ServeHTTP(rec, req); close(done) }()

	m.waitingBackend(t)
	cancel() // the client hangs up
	<-done
	release()

	if rec.Code != 499 {
		t.Fatalf("status = %d, want 499 (client closed request)", rec.Code)
	}
	m.assertNoTrace(t, key, "100.00")
	m.retrySucceeds(t, key, body, "90.00")
}
