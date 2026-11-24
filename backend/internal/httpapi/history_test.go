package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

type historyBody struct {
	Payments []struct {
		PaymentID     string  `json:"payment_id"`
		Status        string  `json:"status"`
		FailureReason *string `json:"failure_reason"`
		Amount        string  `json:"amount"`
	} `json:"payments"`
	NextCursor *string `json:"next_cursor"`
}

func (m *moneyEnv) history(t *testing.T, token, account, query string) (int, historyBody, string) {
	t.Helper()
	rec := m.do(t, http.MethodGet, "/api/v1/accounts/"+account+"/payments"+query, token, nil)
	var body historyBody
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return rec.Code, body, ""
	}
	return rec.Code, body, decodeError(t, rec).Code
}

func TestGetPayment(t *testing.T) {
	m := setupMoney(t)
	_, carolToken := m.createUser(t, "carol@example.com", "USER")
	m.topUp(t, m.adminToken, m.aliceAcct, "100.00")
	paid := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "25.00")
	failed := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "1000.00")

	get := func(token, id string) (int, map[string]any, string) {
		rec := m.do(t, http.MethodGet, "/api/v1/payments/"+id, token, nil)
		if rec.Code != http.StatusOK {
			e := decodeError(t, rec)
			return rec.Code, nil, e.RequestID
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return rec.Code, body, ""
	}

	for _, tc := range []struct{ name, token string }{
		{"sender", m.aliceToken},
		{"receiver", m.bobToken},
	} {
		t.Run(tc.name+" can see it", func(t *testing.T) {
			code, body, _ := get(tc.token, paid.payment.PaymentID)
			if code != http.StatusOK || body["amount"] != "25.00" || body["status"] != "SUCCESS" {
				t.Fatalf("got %d %v", code, body)
			}
			if _, ok := body["failure_reason"]; ok {
				t.Fatal("successful payment should not carry failure_reason")
			}
		})
	}

	t.Run("failed payment shows its reason", func(t *testing.T) {
		code, body, _ := get(m.aliceToken, failed.paymentID)
		if code != http.StatusOK || body["status"] != "FAILED" || body["failure_reason"] != "INSUFFICIENT_FUNDS" {
			t.Fatalf("got %d %v", code, body)
		}
	})

	for _, tc := range []struct {
		name, token, id string
		audited         bool
	}{
		{"unrelated user", carolToken, paid.payment.PaymentID, true},
		{"missing payment", m.aliceToken, "00000000-0000-4000-8000-000000000000", false},
		{"malformed id", m.aliceToken, "nope", false},
	} {
		t.Run(tc.name+" gets 404", func(t *testing.T) {
			code, _, requestID := get(tc.token, tc.id)
			if code != http.StatusNotFound {
				t.Fatalf("status = %d", code)
			}
			want := 0
			if tc.audited {
				want = 1
			}
			if n := m.auditCount(t, "authz.denied", requestID); n != want {
				t.Fatalf("authz.denied audit = %d, want %d", n, want)
			}
		})
	}
}

func TestAccountPaymentHistory(t *testing.T) {
	m := setupMoney(t)
	m.topUp(t, m.adminToken, m.aliceAcct, "100.00")
	var want []string
	for _, amount := range []string{"1.00", "2.00", "3.00", "4.00", "5.00"} {
		r := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, amount)
		want = append([]string{r.payment.PaymentID}, want...) // newest first
	}
	failed := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "999.00")
	want = append([]string{failed.paymentID}, want...)

	t.Run("pages follow next_cursor to the end", func(t *testing.T) {
		var got []string
		query := "?limit=4"
		for i := 0; ; i++ {
			code, body, errCode := m.history(t, m.aliceToken, m.aliceAcct, query)
			if code != http.StatusOK {
				t.Fatalf("page %d: %d %s", i, code, errCode)
			}
			for _, p := range body.Payments {
				got = append(got, p.PaymentID)
			}
			if body.NextCursor == nil {
				break
			}
			query = "?limit=4&cursor=" + url.QueryEscape(*body.NextCursor)
		}
		// alice's history also contains the top-up, which is the oldest.
		if len(got) != len(want)+1 || !equal(got[:len(want)], want) {
			t.Fatalf("history = %v\nwant prefix %v", got, want)
		}
	})

	t.Run("default limit and failure reason", func(t *testing.T) {
		code, body, _ := m.history(t, m.aliceToken, m.aliceAcct, "")
		if code != http.StatusOK || len(body.Payments) != 7 || body.NextCursor != nil {
			t.Fatalf("got %d, %d payments, next=%v", code, len(body.Payments), body.NextCursor)
		}
		if p := body.Payments[0]; p.Status != "FAILED" || p.FailureReason == nil || *p.FailureReason != "INSUFFICIENT_FUNDS" {
			t.Fatalf("newest = %+v", p)
		}
	})

	t.Run("receiver sees received payments", func(t *testing.T) {
		code, body, _ := m.history(t, m.bobToken, m.bobAcct, "")
		// bob received the 5 successful transfers and the failed attempt.
		if code != http.StatusOK || len(body.Payments) != 6 {
			t.Fatalf("got %d, %d payments", code, len(body.Payments))
		}
	})

	t.Run("empty history is an empty list", func(t *testing.T) {
		_, token := m.createUser(t, "dave@example.com", "USER")
		acct := m.createAccount(t, token).AccountID
		rec := m.do(t, http.MethodGet, "/api/v1/accounts/"+acct+"/payments", token, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"payments\":[],\"next_cursor\":null}\n" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	for _, tc := range []struct{ name, query, code string }{
		{"limit zero", "?limit=0", "INVALID_LIMIT"},
		{"limit too high", "?limit=101", "INVALID_LIMIT"},
		{"limit not a number", "?limit=ten", "INVALID_LIMIT"},
		{"tampered cursor", "?cursor=abc", "INVALID_CURSOR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errCode := m.history(t, m.aliceToken, m.aliceAcct, tc.query)
			if code != http.StatusBadRequest || errCode != tc.code {
				t.Fatalf("got %d %s, want 400 %s", code, errCode, tc.code)
			}
		})
	}

	t.Run("someone else's account is 404", func(t *testing.T) {
		code, _, errCode := m.history(t, m.bobToken, m.aliceAcct, "")
		if code != http.StatusNotFound || errCode != "ACCOUNT_NOT_FOUND" {
			t.Fatalf("got %d %s", code, errCode)
		}
	})
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
