package payments_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
)

// Every successful money movement writes exactly one outbox event in the
// same transaction; failed payments write none (Blueprint §8.5).
func TestSuccessfulPaymentsWriteExactlyOneEvent(t *testing.T) {
	f := setup(t)
	alice := f.user("alice@example.com", auth.RoleUser)
	bob := f.user("bob@example.com", auth.RoleUser)
	aliceAcct, bobAcct := f.account(alice), f.account(bob)

	top := f.topUp(aliceAcct, 5000)
	p, err := f.transfer(alice, aliceAcct, bobAcct, 1234)
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := f.transfer(alice, aliceAcct, bobAcct, 999999) // INSUFFICIENT_FUNDS

	event := func(paymentID string) (string, payments.SucceededEvent, int) {
		t.Helper()
		var typ string
		var raw []byte
		var n int
		err := f.db.Owner.QueryRow(context.Background(), `
			SELECT coalesce(min(event_type), ''), coalesce(min(payload::text), '')::bytea, count(*)
			FROM outbox_events WHERE aggregate_id = $1`, paymentID).Scan(&typ, &raw, &n)
		if err != nil {
			t.Fatal(err)
		}
		var ev payments.SucceededEvent
		if n > 0 {
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatal(err)
			}
		}
		return typ, ev, n
	}

	if typ, ev, n := event(top.ID); n != 1 || typ != "topup.succeeded" || ev.Type != payments.TopUp ||
		ev.ReceiverAccountID != aliceAcct || ev.AmountMinor != 5000 || ev.InitiatedBy != f.admin {
		t.Fatalf("top-up event: n=%d type=%s payload=%+v", n, typ, ev)
	}
	if typ, ev, n := event(p.ID); n != 1 || typ != "payment.succeeded" || ev.PaymentID != p.ID ||
		ev.SenderAccountID != aliceAcct || ev.ReceiverAccountID != bobAcct || ev.AmountMinor != 1234 ||
		ev.Currency != "INR" || ev.InitiatedBy != alice {
		t.Fatalf("transfer event: n=%d type=%s payload=%+v", n, typ, ev)
	}
	if _, _, n := event(failed.ID); n != 0 {
		t.Fatalf("FAILED payment wrote %d events, want 0", n)
	}
	if n := f.count(`SELECT count(*) FROM outbox_events`); n != 2 {
		t.Fatalf("outbox events = %d, want 2", n)
	}
}
