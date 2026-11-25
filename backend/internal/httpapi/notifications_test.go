package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/notifications"
	"github.com/nandini7486/fincore/backend/internal/outbox"
)

type notificationsBody struct {
	Notifications []struct {
		NotificationID string `json:"notification_id"`
		Message        string `json:"message"`
		PaymentID      string `json:"payment_id"`
		CreatedAt      string `json:"created_at"`
	} `json:"notifications"`
	NextCursor *string `json:"next_cursor"`
}

// runWorker delivers all pending outbox events, as cmd/worker would.
func (m *moneyEnv) runWorker(t *testing.T) int {
	t.Helper()
	w := &outbox.Worker{DB: m.db.App, Handlers: notifications.Handlers()}
	n, err := w.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (m *moneyEnv) notifications(t *testing.T, token, query string) (int, notificationsBody, string) {
	t.Helper()
	rec := m.do(t, http.MethodGet, "/api/v1/notifications"+query, token, nil)
	var body notificationsBody
	if rec.Code != http.StatusOK {
		return rec.Code, body, decodeError(t, rec).Code
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return rec.Code, body, ""
}

func TestNotificationsEndpoint(t *testing.T) {
	m := setupMoney(t)
	top := m.topUp(t, m.adminToken, m.aliceAcct, "100.00")
	var paid []string // newest first
	for _, amt := range []string{"1.00", "2.00", "3.00", "4.00", "5.00"} {
		r := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, amt)
		paid = append([]string{r.payment.PaymentID}, paid...)
	}
	m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "500.00") // FAILED: no notification

	t.Run("nothing before the worker runs", func(t *testing.T) {
		_, body, _ := m.notifications(t, m.bobToken, "")
		if len(body.Notifications) != 0 {
			t.Fatalf("bob has %d notifications before delivery", len(body.Notifications))
		}
	})

	if n := m.runWorker(t); n != 6 {
		t.Fatalf("worker delivered %d events, want 6 (1 top-up + 5 transfers)", n)
	}

	t.Run("receiver sees their notifications newest first", func(t *testing.T) {
		code, body, _ := m.notifications(t, m.bobToken, "")
		if code != http.StatusOK || len(body.Notifications) != 5 || body.NextCursor != nil {
			t.Fatalf("got %d, %d notifications, next=%v", code, len(body.Notifications), body.NextCursor)
		}
		if got := body.Notifications[0]; got.Message != "You received ₹5.00 from Test." || got.PaymentID != paid[0] || got.NotificationID == "" || got.CreatedAt == "" {
			t.Fatalf("newest = %+v", got)
		}
		for i, n := range body.Notifications {
			if n.PaymentID != paid[i] {
				t.Fatalf("notification %d is for payment %s, want %s (newest first)", i, n.PaymentID, paid[i])
			}
		}
	})

	t.Run("each user sees only their own", func(t *testing.T) {
		_, body, _ := m.notifications(t, m.aliceToken, "")
		if len(body.Notifications) != 1 || body.Notifications[0].Message != "₹100.00 was added to your account." ||
			body.Notifications[0].PaymentID != top.payment.PaymentID {
			t.Fatalf("alice's notifications = %+v", body.Notifications)
		}
		_, body, _ = m.notifications(t, m.adminToken, "")
		if len(body.Notifications) != 0 {
			t.Fatalf("admin has %d notifications, want 0", len(body.Notifications))
		}
	})

	t.Run("pages follow next_cursor without gaps or duplicates", func(t *testing.T) {
		var got []string
		query := "?limit=2"
		for i := 0; ; i++ {
			code, body, errCode := m.notifications(t, m.bobToken, query)
			if code != http.StatusOK {
				t.Fatalf("page %d: %d %s", i, code, errCode)
			}
			if len(body.Notifications) > 2 {
				t.Fatalf("page %d has %d items, limit 2", i, len(body.Notifications))
			}
			for _, n := range body.Notifications {
				got = append(got, n.PaymentID)
			}
			if body.NextCursor == nil {
				break
			}
			query = "?limit=2&cursor=" + url.QueryEscape(*body.NextCursor)
		}
		if !equal(got, paid) {
			t.Fatalf("paged = %v\nwant    %v", got, paid)
		}
	})

	t.Run("empty list for a user with none", func(t *testing.T) {
		_, token := m.createUser(t, "dave@example.com", "USER")
		rec := m.do(t, http.MethodGet, "/api/v1/notifications", token, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"notifications\":[],\"next_cursor\":null}\n" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	for _, tc := range []struct{ name, query, code string }{
		{"limit zero", "?limit=0", "INVALID_LIMIT"},
		{"limit too high", "?limit=101", "INVALID_LIMIT"},
		{"tampered cursor", "?cursor=abc", "INVALID_CURSOR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errCode := m.notifications(t, m.bobToken, tc.query)
			if code != http.StatusBadRequest || errCode != tc.code {
				t.Fatalf("got %d %s, want 400 %s", code, errCode, tc.code)
			}
		})
	}

	// A cursor taken from bob's list cannot reveal alice's notifications:
	// the query is always scoped to the caller.
	t.Run("a cursor cannot cross users", func(t *testing.T) {
		_, first, _ := m.notifications(t, m.bobToken, "?limit=1")
		_, body, _ := m.notifications(t, m.aliceToken, "?cursor="+url.QueryEscape(*first.NextCursor))
		for _, n := range body.Notifications {
			if n.PaymentID != top.payment.PaymentID {
				t.Fatalf("alice saw a notification that is not hers: %+v", n)
			}
		}
	})
}
