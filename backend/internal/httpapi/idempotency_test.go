package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func (m *moneyEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := m.db.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fundedAlice tops up alice's account with ₹100.00.
func fundedAlice(t *testing.T) *moneyEnv {
	t.Helper()
	m := setupMoney(t)
	if r := m.topUp(t, m.adminToken, m.aliceAcct, "100.00"); r.code != http.StatusCreated {
		t.Fatalf("top-up = %d", r.code)
	}
	return m
}

func TestRetryWithSameKeyIsReplayedNotCharged(t *testing.T) {
	m := fundedAlice(t)
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	first := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
	second := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))

	if first.code != http.StatusCreated || first.replayed {
		t.Fatalf("first = %d replayed=%v", first.code, first.replayed)
	}
	if second.code != http.StatusCreated || !second.replayed {
		t.Fatalf("retry = %d replayed=%v, want 201 replayed", second.code, second.replayed)
	}
	if first.payment.PaymentID == "" || second.payment.PaymentID != first.payment.PaymentID {
		t.Fatalf("payment ids %q vs %q", first.payment.PaymentID, second.payment.PaymentID)
	}
	if len(second.raw) != len(first.raw) {
		t.Fatalf("replayed body differs:\n%v\n%v", first.raw, second.raw)
	}
	for k, v := range first.raw {
		if second.raw[k] != v {
			t.Fatalf("replayed body field %q = %v, want %v", k, second.raw[k], v)
		}
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "90.00" {
		t.Fatalf("alice = %s, want 90.00 (charged once)", got)
	}
	if n := m.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 1 {
		t.Fatalf("transfers = %d, want 1", n)
	}
}

// The fingerprint uses parsed values, so a semantically identical retry is a
// replay even if the client formats it differently.
func TestEquivalentRequestIsReplayed(t *testing.T) {
	m := fundedAlice(t)
	key := newKey()

	first := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key,
		payBody(m.aliceAcct, m.bobAcct, "5.00")))
	second := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key,
		payBody(strings.ToUpper(m.aliceAcct), strings.ToUpper(m.bobAcct), "5.0")))

	if first.code != http.StatusCreated || second.code != http.StatusCreated || !second.replayed ||
		second.payment.PaymentID != first.payment.PaymentID {
		t.Fatalf("first=%d second=%d replayed=%v", first.code, second.code, second.replayed)
	}
}

func TestSameKeyDifferentRequestIsConflict(t *testing.T) {
	m := fundedAlice(t)
	key := newKey()

	if r := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, "10.00"))); r.code != http.StatusCreated {
		t.Fatalf("first = %d", r.code)
	}
	r := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, "20.00")))
	if r.code != http.StatusConflict || r.errCode != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("got %d %s, want 409 IDEMPOTENCY_KEY_REUSED", r.code, r.errCode)
	}
	if n := m.auditCount(t, "idempotency.conflict", r.requestID); n != 1 {
		t.Fatalf("idempotency.conflict audit = %d, want 1", n)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "90.00" {
		t.Fatalf("alice = %s, want 90.00", got)
	}
}

// A key used for a top-up cannot be replayed as a payment: the operation is
// part of the fingerprint.
func TestSameKeyOnDifferentEndpointIsConflict(t *testing.T) {
	m := setupMoney(t)
	adminAcct := m.createAccount(t, m.adminToken).AccountID
	key := newKey()

	if r := result(m.moneyPost(t, "/api/v1/accounts/"+adminAcct+"/topups", m.adminToken, key, topUpBody("10.00"))); r.code != http.StatusCreated {
		t.Fatalf("top-up = %d", r.code)
	}
	r := result(m.moneyPost(t, "/api/v1/payments", m.adminToken, key, payBody(adminAcct, m.bobAcct, "10.00")))
	if r.code != http.StatusConflict {
		t.Fatalf("payment with top-up's key = %d, want 409", r.code)
	}
}

// A business-rule failure is a final answer: retrying replays the same 422
// and the same FAILED payment, never a second attempt.
func TestRuleFailureIsReplayed(t *testing.T) {
	m := fundedAlice(t)
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "500.00")

	first := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
	// Even if alice now has enough money, the stored outcome is replayed.
	m.topUp(t, m.adminToken, m.aliceAcct, "1000.00")
	second := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))

	if first.code != http.StatusUnprocessableEntity || first.errCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("first = %d %s", first.code, first.errCode)
	}
	if second.code != http.StatusUnprocessableEntity || !second.replayed || second.paymentID != first.paymentID {
		t.Fatalf("retry = %d replayed=%v payment=%q, want replay of %q", second.code, second.replayed, second.paymentID, first.paymentID)
	}
	if second.requestID == "" || second.raw["error"].(map[string]any)["request_id"] == second.requestID {
		t.Fatal("replayed body should carry the original request_id, not the retry's")
	}
	if n := m.count(t, `SELECT count(*) FROM payments WHERE status = 'FAILED'`); n != 1 {
		t.Fatalf("FAILED payments = %d, want 1", n)
	}
}

func TestKeysAreScopedPerUser(t *testing.T) {
	m := fundedAlice(t)
	m.topUp(t, m.adminToken, m.bobAcct, "100.00")
	key := newKey()

	a := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, "1.00")))
	b := result(m.moneyPost(t, "/api/v1/payments", m.bobToken, key, payBody(m.bobAcct, m.aliceAcct, "2.00")))
	if a.code != http.StatusCreated || b.code != http.StatusCreated || b.replayed || a.payment.PaymentID == b.payment.PaymentID {
		t.Fatalf("alice=%d bob=%d replayed=%v", a.code, b.code, b.replayed)
	}
}

func TestIdempotencyKeyIsRequiredAndValidated(t *testing.T) {
	m := fundedAlice(t)
	body := payBody(m.aliceAcct, m.bobAcct, "1.00")

	for _, tc := range []struct{ name, key, code string }{
		{"missing", "", "IDEMPOTENCY_KEY_REQUIRED"},
		{"too long", strings.Repeat("k", 256), "INVALID_IDEMPOTENCY_KEY"},
		{"contains a space", "my key", "INVALID_IDEMPOTENCY_KEY"},
		{"non-ASCII", "clé", "INVALID_IDEMPOTENCY_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, tc.key, body))
			if r.code != http.StatusBadRequest || r.errCode != tc.code {
				t.Fatalf("got %d %s, want 400 %s", r.code, r.errCode, tc.code)
			}
		})
	}
	r := result(m.moneyPost(t, "/api/v1/accounts/"+m.aliceAcct+"/topups", m.adminToken, "", topUpBody("1.00")))
	if r.code != http.StatusBadRequest || r.errCode != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("top-up without key = %d %s", r.code, r.errCode)
	}
	if n := m.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 0 {
		t.Fatalf("transfers = %d, want 0", n)
	}
}

// After 24 hours a key may be reused for a new request (NFR-01).
func TestExpiredKeyCanBeReused(t *testing.T) {
	m := fundedAlice(t)
	key := newKey()

	first := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, "1.00")))
	if _, err := m.db.Owner.Exec(context.Background(),
		`UPDATE idempotency_records SET expires_at = now() - interval '1 second' WHERE key = $1`, key); err != nil {
		t.Fatal(err)
	}
	second := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, "2.00")))

	if first.code != http.StatusCreated || second.code != http.StatusCreated || second.replayed ||
		second.payment.PaymentID == first.payment.PaymentID {
		t.Fatalf("first=%d second=%d replayed=%v", first.code, second.code, second.replayed)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "97.00" {
		t.Fatalf("alice = %s, want 97.00", got)
	}
}

// A request that fails before commit (here: lock timeout, 503) leaves no
// trace, so retrying with the same key is processed normally.
func TestFailedAttemptDoesNotBurnTheKey(t *testing.T) {
	m := fundedAlice(t)
	ctx := context.Background()
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	blocker, err := m.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, m.aliceAcct); err != nil {
		t.Fatal(err)
	}
	busy := m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body)
	_ = blocker.Rollback(ctx)

	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") == "" {
		t.Fatalf("while locked = %d, want 503 with Retry-After", busy.Code)
	}
	if n := m.count(t, `SELECT count(*) FROM idempotency_records WHERE key = $1`, key); n != 0 {
		t.Fatalf("idempotency records after 503 = %d, want 0", n)
	}
	retry := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
	if retry.code != http.StatusCreated || retry.replayed {
		t.Fatalf("retry = %d replayed=%v, want a fresh 201", retry.code, retry.replayed)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "90.00" {
		t.Fatalf("alice = %s, want 90.00", got)
	}
}

// The core guarantee: many copies of one request arriving at the same time
// (a client retrying aggressively, or a double-tapped button) move money
// exactly once, and every caller gets the same answer.
func TestConcurrentDuplicatesMoveMoneyOnce(t *testing.T) {
	m := fundedAlice(t)
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	const copies = 12
	results := make([]*httpResult, copies)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	for i, r := range results {
		if r.code != http.StatusCreated {
			t.Fatalf("copy %d = %d %s", i, r.code, r.errCode)
		}
		if r.payment.PaymentID != results[0].payment.PaymentID {
			t.Fatalf("copy %d got payment %s, want %s", i, r.payment.PaymentID, results[0].payment.PaymentID)
		}
		if !r.replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d copies were processed, want exactly 1 (the rest replayed)", fresh)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "90.00" {
		t.Fatalf("alice = %s, want 90.00 (charged once)", got)
	}
	if n := m.count(t, `SELECT count(*) FROM ledger_entries e JOIN payments p ON p.id = e.payment_id WHERE p.type = 'TRANSFER'`); n != 2 {
		t.Fatalf("transfer ledger entries = %d, want 2", n)
	}
}

// Concurrent requests with one key but different content: one wins, every
// other gets 409. None is silently merged or double-processed.
func TestConcurrentConflictingRequests(t *testing.T) {
	m := fundedAlice(t)
	key := newKey()

	amounts := []string{"1.00", "2.00", "3.00", "4.00", "5.00", "6.00"}
	codes := make([]int, len(amounts))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, amt := range amounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, payBody(m.aliceAcct, m.bobAcct, amt))).code
		}()
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 || conflicts != len(amounts)-1 {
		t.Fatalf("created=%d conflicts=%d, want 1 and %d", created, conflicts, len(amounts)-1)
	}
	if n := m.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 1 {
		t.Fatalf("transfers = %d, want 1", n)
	}
}

// Deterministic version of the race above. The account row is held locked
// while the duplicates arrive, so the first request is guaranteed to be
// mid-transaction when the others reach the claim. A naive "check if the key
// exists, then insert" implementation double-charges here every time; the
// in-transaction claim makes the duplicates wait and then replay.
func TestDuplicatesWhileFirstRequestIsInFlight(t *testing.T) {
	m := fundedAlice(t)
	ctx := context.Background()
	key, body := newKey(), payBody(m.aliceAcct, m.bobAcct, "10.00")

	blocker, err := m.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, m.aliceAcct); err != nil {
		t.Fatal(err)
	}

	const copies = 12
	results := make([]*httpResult, copies)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, key, body))
		}()
	}
	// Let every copy reach the database and queue up, well within the 2s
	// lock_timeout, then let the first one proceed.
	time.Sleep(500 * time.Millisecond)
	_ = blocker.Rollback(ctx)
	wg.Wait()

	fresh := 0
	for i, r := range results {
		if r.code != http.StatusCreated || r.payment.PaymentID != results[0].payment.PaymentID {
			t.Fatalf("copy %d = %d payment %q, want 201 payment %q", i, r.code, r.payment.PaymentID, results[0].payment.PaymentID)
		}
		if !r.replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d copies were processed, want exactly 1", fresh)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "90.00" {
		t.Fatalf("alice = %s, want 90.00 (charged once)", got)
	}
}
