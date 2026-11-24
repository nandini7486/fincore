package notifications_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/notifications"
	"github.com/nandini7486/fincore/backend/internal/outbox"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/cursor"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

type world struct {
	t                  *testing.T
	db                 *testdb.DB
	admin, alice, bob  string
	aliceAcct, bobAcct string
	worker             *outbox.Worker
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, db: testdb.New(t)}
	user := func(name, email string, role auth.Role) string {
		var id auth.Identity
		err := db.WithTx(context.Background(), w.db.Owner, func(tx pgx.Tx) error {
			var err error
			id, _, err = users.Create(context.Background(), tx, users.NewUser{Name: name, Email: email, Role: role})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id.UserID
	}
	account := func(owner string) string {
		var a accounts.Account
		err := db.WithTx(context.Background(), w.db.App, func(tx pgx.Tx) error {
			var err error
			a, err = accounts.Create(context.Background(), tx, owner, "")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	w.admin = user("Admin", "admin@example.com", auth.RoleAdmin)
	w.alice = user("Alice Sharma", "alice@example.com", auth.RoleUser)
	w.bob = user("Bob", "bob@example.com", auth.RoleUser)
	w.aliceAcct, w.bobAcct = account(w.alice), account(w.bob)
	w.worker = &outbox.Worker{DB: w.db.App, Handlers: notifications.Handlers()}
	return w
}

func (w *world) move(fn func(context.Context, pgx.Tx, payments.Request) (payments.Payment, error), r payments.Request) {
	w.t.Helper()
	var ruleErr *payments.RuleError
	err := db.WithTx(context.Background(), w.db.App, func(tx pgx.Tx) error {
		_, err := fn(context.Background(), tx, r)
		if errors.As(err, &ruleErr) {
			return nil
		}
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) messages(userID string) []string {
	w.t.Helper()
	rows, err := w.db.Owner.Query(context.Background(),
		`SELECT message FROM notifications WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		w.t.Fatal(err)
	}
	msgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		w.t.Fatal(err)
	}
	return msgs
}

func (w *world) drain() int {
	w.t.Helper()
	n, err := w.worker.Drain(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestPaymentsBecomeNotificationsForTheReceiver(t *testing.T) {
	w := newWorld(t)
	w.move(payments.CreateTopUp, payments.Request{ReceiverID: w.aliceAcct, Amount: 50000000, Currency: "INR", InitiatedBy: w.admin})
	w.move(payments.CreateTransfer, payments.Request{SenderID: w.aliceAcct, ReceiverID: w.bobAcct, Amount: 200000, Currency: "INR", InitiatedBy: w.alice})
	w.move(payments.CreateTransfer, payments.Request{SenderID: w.bobAcct, ReceiverID: w.aliceAcct, Amount: 999999999, Currency: "INR", InitiatedBy: w.bob}) // FAILED: no event

	// Nothing is delivered until the worker runs.
	if got := w.messages(w.bob); len(got) != 0 {
		t.Fatalf("bob has notifications before the worker ran: %v", got)
	}
	if n := w.drain(); n != 2 {
		t.Fatalf("worker processed %d events, want 2 (top-up and transfer)", n)
	}

	if got := w.messages(w.alice); len(got) != 1 || got[0] != "₹5,00,000.00 was added to your account." {
		t.Fatalf("alice's notifications = %q", got)
	}
	if got := w.messages(w.bob); len(got) != 1 || got[0] != "You received ₹2,000.00 from Alice Sharma." {
		t.Fatalf("bob's notifications = %q", got)
	}
	// The sender and the admin are not notified in V1.
	if got := w.messages(w.admin); len(got) != 0 {
		t.Fatalf("admin notifications = %q", got)
	}
}

// At-least-once delivery means a handler can see an event twice. The UNIQUE
// (event_id) constraint turns the second delivery into a no-op.
func TestRedeliveredEventDoesNotDuplicateNotification(t *testing.T) {
	w := newWorld(t)
	w.move(payments.CreateTopUp, payments.Request{ReceiverID: w.aliceAcct, Amount: 1000, Currency: "INR", InitiatedBy: w.admin})
	w.drain()

	// Simulate redelivery: mark every event pending again, twice over.
	for i := 0; i < 2; i++ {
		if _, err := w.db.Owner.Exec(context.Background(), `UPDATE outbox_events SET processed_at = NULL`); err != nil {
			t.Fatal(err)
		}
		if n := w.drain(); n != 1 {
			t.Fatalf("redelivery processed %d, want 1", n)
		}
	}
	if got := w.messages(w.alice); len(got) != 1 {
		t.Fatalf("alice has %d notifications after 3 deliveries, want 1: %q", len(got), got)
	}
	var attempts int
	if err := w.db.Owner.QueryRow(context.Background(), `SELECT attempts FROM outbox_events`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 deliveries recorded", attempts)
	}
}

func TestMessage(t *testing.T) {
	name := "Alice"
	empty := ""
	for _, tc := range []struct {
		ev     payments.SucceededEvent
		sender *string
		want   string
	}{
		{payments.SucceededEvent{Type: payments.Transfer, AmountMinor: 200000}, &name, "You received ₹2,000.00 from Alice."},
		{payments.SucceededEvent{Type: payments.Transfer, AmountMinor: 1}, nil, "You received ₹0.01."},
		{payments.SucceededEvent{Type: payments.Transfer, AmountMinor: 1}, &empty, "You received ₹0.01."},
		{payments.SucceededEvent{Type: payments.TopUp, AmountMinor: 12345678}, nil, "₹1,23,456.78 was added to your account."},
	} {
		if got := notifications.Message(tc.ev, tc.sender); got != tc.want {
			t.Errorf("Message(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

// Bad events are handler errors: recorded with a clear reason and retried
// later, never a crash.
func TestBadEventsAreRecordedNotCrashing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{
			name:    "incomplete payload",
			payload: map[string]any{"receiver_account_id": "00000000-0000-4000-8000-000000000009", "amount_minor": 100},
			wantErr: "payment.succeeded payload is incomplete or invalid",
		},
		{
			name: "receiver does not exist",
			payload: map[string]any{
				"type": "TRANSFER", "amount_minor": 100, "currency": "INR",
				"sender_account_id":   "00000000-0000-4000-8000-000000000008",
				"receiver_account_id": "00000000-0000-4000-8000-000000000009",
			},
			wantErr: "receiver account 00000000-0000-4000-8000-000000000009 not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			err := db.WithTx(context.Background(), w.db.App, func(tx pgx.Tx) error {
				return outbox.Write(context.Background(), tx, outbox.PaymentSucceeded, "00000000-0000-4000-8000-000000000001", tc.payload)
			})
			if err != nil {
				t.Fatal(err)
			}
			w.drain()
			var lastErr *string
			var processed bool
			if err := w.db.Owner.QueryRow(context.Background(),
				`SELECT last_error, processed_at IS NOT NULL FROM outbox_events`).Scan(&lastErr, &processed); err != nil {
				t.Fatal(err)
			}
			if processed || lastErr == nil || *lastErr != tc.wantErr {
				got := "<nil>"
				if lastErr != nil {
					got = *lastErr
				}
				t.Fatalf("processed=%v last_error=%q, want %q", processed, got, tc.wantErr)
			}
		})
	}
}

// Notifications created in the same microsecond still page correctly: the
// cursor's id tie-breaker applies to this query too.
func TestListBreaksTimestampTiesByID(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// Five events and notifications for bob, all with one identical timestamp.
	if _, err := w.db.Owner.Exec(ctx, `
		WITH ev AS (
			INSERT INTO outbox_events (event_type, aggregate_id, payload, processed_at)
			SELECT 'payment.succeeded', gen_random_uuid(), '{}', now() FROM generate_series(1, 5)
			RETURNING id
		)
		INSERT INTO notifications (user_id, event_id, message, created_at)
		SELECT $1, id, 'tie', '2026-01-01T10:00:00Z' FROM ev`, w.bob); err != nil {
		t.Fatal(err)
	}
	rows, err := w.db.Owner.Query(ctx, `SELECT id FROM notifications WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, w.bob)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{1, 2, 4} {
		var got []string
		var after *cursor.Cursor
		for pages := 0; pages < 10; pages++ {
			page, next, err := notifications.List(ctx, w.db.App, w.bob, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range page {
				got = append(got, n.ID)
			}
			if next == nil {
				break
			}
			after = next
		}
		if len(got) != len(want) {
			t.Fatalf("limit %d: got %d notifications %v, want %d", limit, len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("limit %d: order %v, want %v", limit, got, want)
			}
		}
	}
}
