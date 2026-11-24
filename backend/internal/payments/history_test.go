package payments_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
)

// allHistory pages through an account's history and returns the payment IDs
// in the order received, failing on any duplicate.
func (f *fixture) allHistory(account string, limit int) []string {
	f.t.Helper()
	var got []string
	seen := map[string]bool{}
	var after *payments.Cursor
	for pages := 0; ; pages++ {
		if pages > 100 {
			f.t.Fatal("pagination did not terminate")
		}
		page, next, err := payments.History(context.Background(), f.db.App, account, after, limit)
		if err != nil {
			f.t.Fatal(err)
		}
		if len(page) > limit {
			f.t.Fatalf("page has %d items, limit %d", len(page), limit)
		}
		for _, p := range page {
			if seen[p.ID] {
				f.t.Fatalf("payment %s returned twice", p.ID)
			}
			seen[p.ID] = true
			got = append(got, p.ID)
		}
		if next == nil {
			return got
		}
		if len(page) != limit {
			f.t.Fatalf("non-final page has %d items, want %d", len(page), limit)
		}
		// Round-trip through the opaque string, as a client would.
		c, err := payments.DecodeCursor(next.Encode())
		if err != nil {
			f.t.Fatal(err)
		}
		after = &c
	}
}

// expectedHistory is the reference answer, computed with a plain query.
func (f *fixture) expectedHistory(account string) []string {
	f.t.Helper()
	rows, err := f.db.Owner.Query(context.Background(), `
		SELECT id FROM payments
		WHERE sender_account_id = $1 OR receiver_account_id = $1
		ORDER BY created_at DESC, id DESC`, account)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHistoryPaginatesNewestFirstWithoutGapsOrDuplicates(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	carol := f.user("carol@example.com", auth.RoleUser)
	aliceAcct, bobAcct, carolAcct := f.account(alice), f.account(bob), f.account(carol)

	f.topUp(aliceAcct, 100000)
	f.topUp(bobAcct, 100000)
	f.topUp(carolAcct, 100000)
	for i := 0; i < 7; i++ {
		if _, err := f.transfer(alice, aliceAcct, bobAcct, 100); err != nil { // sent
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := f.transfer(bob, bobAcct, aliceAcct, 100); err != nil { // received
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		_, _ = f.transfer(alice, aliceAcct, bobAcct, 10_000_000) // FAILED, still history
	}
	for i := 0; i < 4; i++ {
		if _, err := f.transfer(carol, carolAcct, bobAcct, 100); err != nil { // not alice's
			t.Fatal(err)
		}
	}

	want := f.expectedHistory(aliceAcct)
	if len(want) != 1+7+3+2 {
		t.Fatalf("reference history has %d payments, want 13", len(want))
	}
	for _, limit := range []int{1, 2, 3, 5, 13, 100} {
		if got := f.allHistory(aliceAcct, limit); !equal(got, want) {
			t.Fatalf("limit %d: got %v\nwant %v", limit, got, want)
		}
	}

	// Each page item carries what clients display, including failures.
	page, _, err := payments.History(context.Background(), f.db.App, aliceAcct, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page[0].Status != "FAILED" || page[0].FailureReason == nil || *page[0].FailureReason != payments.ReasonInsufficientFunds {
		t.Fatalf("newest payment = %+v, want the FAILED transfer", page[0])
	}
}

// Payments created in the same microsecond must still page correctly: the
// cursor includes the ID as a tie-breaker.
func TestHistoryBreaksTimestampTiesByID(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)

	same := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		// FAILED payments move no money, so the ledger invariants still hold.
		if _, err := f.db.Owner.Exec(ctx, `
			INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
			                      status, failure_reason, initiated_by_user_id, request_id, created_at)
			VALUES ('TRANSFER', $1, $2, 100, 'INR', 'FAILED', 'INSUFFICIENT_FUNDS', $3, 'req_tie', $4)`,
			aliceAcct, bobAcct, alice, same); err != nil {
			t.Fatal(err)
		}
	}

	want := f.expectedHistory(aliceAcct)
	for _, limit := range []int{1, 2, 4} {
		if got := f.allHistory(aliceAcct, limit); !equal(got, want) {
			t.Fatalf("limit %d: got %v\nwant %v", limit, got, want)
		}
	}
}

// New payments arriving while a client pages must not shift later pages
// (the classic OFFSET bug: items repeat or are skipped).
func TestHistoryIsStableWhileNewPaymentsArrive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	f.topUp(aliceAcct, 100000)
	for i := 0; i < 4; i++ {
		if _, err := f.transfer(alice, aliceAcct, bobAcct, 100); err != nil {
			t.Fatal(err)
		}
	}
	before := f.expectedHistory(aliceAcct) // 5 payments, newest first

	page1, next, err := payments.History(ctx, f.db.App, aliceAcct, nil, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Three new payments land between page 1 and page 2.
	for i := 0; i < 3; i++ {
		if _, err := f.transfer(alice, aliceAcct, bobAcct, 100); err != nil {
			t.Fatal(err)
		}
	}

	var rest []string
	for next != nil {
		var page []payments.Payment
		page, next, err = payments.History(ctx, f.db.App, aliceAcct, next, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			rest = append(rest, p.ID)
		}
	}
	got := []string{page1[0].ID, page1[1].ID}
	got = append(got, rest...)
	if !equal(got, before) {
		t.Fatalf("history shifted while paging:\ngot  %v\nwant %v", got, before)
	}
}

func TestGetForUser(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	carol := f.user("carol@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)
	top := f.topUp(aliceAcct, 1000)
	p, err := f.transfer(alice, aliceAcct, bobAcct, 100)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, payment, user string
		want                error
	}{
		{"sender owner", p.ID, alice, nil},
		{"receiver owner", p.ID, bob, nil},
		{"top-up receiver", top.ID, alice, nil},
		{"unrelated user", p.ID, carol, payments.ErrPaymentNotVisible},
		{"top-up, unrelated user", top.ID, bob, payments.ErrPaymentNotVisible},
		{"missing payment", "00000000-0000-4000-8000-000000000000", alice, payments.ErrPaymentNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := payments.GetForUser(ctx, f.db.App, tc.payment, tc.user)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && got.ID != tc.payment {
				t.Fatalf("got payment %s, want %s", got.ID, tc.payment)
			}
		})
	}
}

func TestCursor(t *testing.T) {
	c := payments.Cursor{
		CreatedAt: time.Date(2026, 11, 24, 7, 47, 10, 91775000, time.UTC),
		ID:        "1f311ea0-c545-4b46-9bc6-c167e8eedaaa",
	}
	got, err := payments.DecodeCursor(c.Encode())
	if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != c.ID {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, c)
	}

	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"",
		"%%%not-base64",
		enc("no separator"),
		enc("2026-11-24T07:47:10Z|not-a-uuid"),
		enc("yesterday|1f311ea0-c545-4b46-9bc6-c167e8eedaaa"),
		enc("2026-11-24T07:47:10Z|1f311ea0-c545-4b46-9bc6-c167e8eedaaa' OR '1'='1"),
	} {
		if _, err := payments.DecodeCursor(bad); !errors.Is(err, payments.ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) err = %v, want ErrInvalidCursor", bad, err)
		}
	}
}
