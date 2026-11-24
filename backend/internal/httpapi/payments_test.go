package httpapi_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/auth"
)

type paymentBody struct {
	PaymentID         string `json:"payment_id"`
	Type              string `json:"type"`
	SenderAccountID   string `json:"sender_account_id"`
	ReceiverAccountID string `json:"receiver_account_id"`
	Amount            string `json:"amount"`
	Currency          string `json:"currency"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
}

type moneyEnv struct {
	*env
	adminToken, aliceToken, bobToken string
	aliceAcct, bobAcct, treasury     string
}

func setupMoney(t *testing.T) *moneyEnv {
	t.Helper()
	m := &moneyEnv{env: setup(t)}
	_, m.adminToken = m.createUser(t, "admin@example.com", auth.RoleAdmin)
	_, m.aliceToken = m.createUser(t, "alice@example.com", auth.RoleUser)
	_, m.bobToken = m.createUser(t, "bob@example.com", auth.RoleUser)
	m.aliceAcct = m.createAccount(t, m.aliceToken).AccountID
	m.bobAcct = m.createAccount(t, m.bobToken).AccountID
	if err := m.db.Owner.QueryRow(context.Background(),
		`SELECT id FROM accounts WHERE type = 'TREASURY'`).Scan(&m.treasury); err != nil {
		t.Fatal(err)
	}
	return m
}

// newKey returns a fresh Idempotency-Key, as a client would per payment.
func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// moneyPost sends a POST with an Idempotency-Key ("" sends none).
func (m *moneyEnv) moneyPost(t *testing.T, path, token, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	return rec
}

func topUpBody(amount string) string {
	return fmt.Sprintf(`{"amount":%q,"currency":"INR"}`, amount)
}

func payBody(from, to, amount string) string {
	return fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":%q,"currency":"INR"}`, from, to, amount)
}

func (m *moneyEnv) topUp(t *testing.T, token, account, amount string) *httpResult {
	t.Helper()
	return result(m.moneyPost(t, "/api/v1/accounts/"+account+"/topups", token, newKey(), topUpBody(amount)))
}

func (m *moneyEnv) pay(t *testing.T, token, from, to, amount string) *httpResult {
	t.Helper()
	return result(m.moneyPost(t, "/api/v1/payments", token, newKey(), payBody(from, to, amount)))
}

func (m *moneyEnv) balance(t *testing.T, token, account string) string {
	t.Helper()
	rec := m.do(t, http.MethodGet, "/api/v1/accounts/"+account, token, nil)
	var a accountBody
	if err := json.NewDecoder(rec.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

type httpResult struct {
	code      int
	requestID string
	payment   paymentBody
	errCode   string
	paymentID string // set on 422
	replayed  bool   // Idempotent-Replayed: true
	raw       map[string]any
}

func result(rec interface {
	Result() *http.Response
}) *httpResult {
	res := rec.Result()
	defer res.Body.Close()
	r := &httpResult{code: res.StatusCode, requestID: res.Header.Get("X-Request-ID"),
		replayed: res.Header.Get("Idempotent-Replayed") == "true"}
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(res.Body).Decode(&raw)
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &r.raw)
	if e, ok := raw["error"]; ok {
		var d struct {
			Code      string `json:"code"`
			PaymentID string `json:"payment_id"`
		}
		_ = json.Unmarshal(e, &d)
		r.errCode, r.paymentID = d.Code, d.PaymentID
		return r
	}
	_ = json.Unmarshal(b, &r.payment)
	return r
}

func TestTopUps(t *testing.T) {
	m := setupMoney(t)

	r := m.topUp(t, m.adminToken, m.aliceAcct, "5000.00")
	if r.code != http.StatusCreated {
		t.Fatalf("top-up = %d %s", r.code, r.errCode)
	}
	p := r.payment
	if p.Type != "TOPUP" || p.Status != "SUCCESS" || p.Amount != "5000.00" || p.Currency != "INR" ||
		p.SenderAccountID != m.treasury || p.ReceiverAccountID != m.aliceAcct || p.PaymentID == "" {
		t.Fatalf("payment = %+v", p)
	}
	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "5000.00" {
		t.Fatalf("alice balance = %s", got)
	}

	for _, tc := range []struct {
		name, token, account, amount string
		code                         int
		errCode                      string
	}{
		{"non-admin", m.aliceToken, m.aliceAcct, "10.00", http.StatusForbidden, "FORBIDDEN"},
		{"missing account", m.adminToken, "00000000-0000-4000-8000-000000000000", "10.00", http.StatusNotFound, "ACCOUNT_NOT_FOUND"},
		{"malformed account id", m.adminToken, "nope", "10.00", http.StatusNotFound, "ACCOUNT_NOT_FOUND"},
		{"treasury itself", m.adminToken, m.treasury, "10.00", http.StatusNotFound, "ACCOUNT_NOT_FOUND"},
		{"invalid amount", m.adminToken, m.aliceAcct, "10.001", http.StatusBadRequest, "INVALID_AMOUNT"},
		{"above maximum", m.adminToken, m.aliceAcct, "10000000.01", http.StatusBadRequest, "INVALID_AMOUNT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := m.topUp(t, tc.token, tc.account, tc.amount)
			if r.code != tc.code || r.errCode != tc.errCode {
				t.Fatalf("got %d %s, want %d %s", r.code, r.errCode, tc.code, tc.errCode)
			}
		})
	}

	if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "5000.00" {
		t.Fatalf("alice balance after rejected top-ups = %s, want 5000.00", got)
	}
}

func TestPayments(t *testing.T) {
	m := setupMoney(t)
	if r := m.topUp(t, m.adminToken, m.aliceAcct, "100.00"); r.code != http.StatusCreated {
		t.Fatalf("top-up = %d", r.code)
	}

	t.Run("success", func(t *testing.T) {
		r := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "40.50")
		if r.code != http.StatusCreated {
			t.Fatalf("status = %d %s", r.code, r.errCode)
		}
		p := r.payment
		if p.Type != "TRANSFER" || p.Status != "SUCCESS" || p.Amount != "40.50" ||
			p.SenderAccountID != m.aliceAcct || p.ReceiverAccountID != m.bobAcct {
			t.Fatalf("payment = %+v", p)
		}
		if a, b := m.balance(t, m.aliceToken, m.aliceAcct), m.balance(t, m.bobToken, m.bobAcct); a != "59.50" || b != "40.50" {
			t.Fatalf("balances alice=%s bob=%s", a, b)
		}
	})

	t.Run("insufficient funds is 422 with a stored payment", func(t *testing.T) {
		r := m.pay(t, m.aliceToken, m.aliceAcct, m.bobAcct, "59.51")
		if r.code != http.StatusUnprocessableEntity || r.errCode != "INSUFFICIENT_FUNDS" || r.paymentID == "" {
			t.Fatalf("got %d %s payment_id=%q", r.code, r.errCode, r.paymentID)
		}
		var status string
		if err := m.db.Owner.QueryRow(context.Background(),
			`SELECT status FROM payments WHERE id = $1`, r.paymentID).Scan(&status); err != nil || status != "FAILED" {
			t.Fatalf("stored payment status = %q, %v", status, err)
		}
		if got := m.balance(t, m.aliceToken, m.aliceAcct); got != "59.50" {
			t.Fatalf("alice balance = %s, want unchanged 59.50", got)
		}
	})

	// Same 404 either way; only a real account owned by someone else (or
	// the treasury) is audited as authz.denied.
	for _, tc := range []struct {
		name, from string
		audited    bool
	}{
		{"someone else's sender account", m.aliceAcct, true},
		{"treasury as sender", m.treasury, true},
		{"missing sender account", "00000000-0000-4000-8000-000000000000", false},
	} {
		t.Run(tc.name+" is 404", func(t *testing.T) {
			r := m.pay(t, m.bobToken, tc.from, m.bobAcct, "1.00")
			if r.code != http.StatusNotFound || r.errCode != "ACCOUNT_NOT_FOUND" {
				t.Fatalf("got %d %s", r.code, r.errCode)
			}
			want := 0
			if tc.audited {
				want = 1
			}
			if n := m.auditCount(t, "authz.denied", r.requestID); n != want {
				t.Fatalf("authz.denied audit records = %d, want %d", n, want)
			}
		})
	}

	t.Run("unknown receiver is 422", func(t *testing.T) {
		r := m.pay(t, m.aliceToken, m.aliceAcct, "00000000-0000-4000-8000-000000000000", "1.00")
		if r.code != http.StatusUnprocessableEntity || r.errCode != "RECEIVER_NOT_FOUND" {
			t.Fatalf("got %d %s", r.code, r.errCode)
		}
	})

	t.Run("unauthenticated is 401", func(t *testing.T) {
		if r := m.pay(t, "", m.aliceAcct, m.bobAcct, "1.00"); r.code != http.StatusUnauthorized {
			t.Fatalf("got %d", r.code)
		}
	})
}

func TestPaymentValidation(t *testing.T) {
	m := setupMoney(t)
	a, b := m.aliceAcct, m.bobAcct

	for _, tc := range []struct{ name, body, code string }{
		{"empty body", ``, "INVALID_REQUEST"},
		{"malformed JSON", `{"amount":`, "INVALID_REQUEST"},
		{"unknown field", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"1.00","currency":"INR","fee":"0"}`, a, b), "INVALID_REQUEST"},
		{"amount as JSON number", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":1.5,"currency":"INR"}`, a, b), "INVALID_REQUEST"},
		{"bad sender id", fmt.Sprintf(`{"sender_account_id":"x","receiver_account_id":%q,"amount":"1.00","currency":"INR"}`, b), "INVALID_ACCOUNT_ID"},
		{"same account", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"1.00","currency":"INR"}`, a, a), "SAME_ACCOUNT"},
		{"zero amount", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"0.00","currency":"INR"}`, a, b), "INVALID_AMOUNT"},
		{"negative amount", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"-1.00","currency":"INR"}`, a, b), "INVALID_AMOUNT"},
		{"three decimals", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"1.005","currency":"INR"}`, a, b), "INVALID_AMOUNT"},
		{"other currency", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"1.00","currency":"USD"}`, a, b), "UNSUPPORTED_CURRENCY"},
		{"missing currency", fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"1.00"}`, a, b), "UNSUPPORTED_CURRENCY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := result(m.moneyPost(t, "/api/v1/payments", m.aliceToken, newKey(), tc.body))
			if r.code != http.StatusBadRequest || r.errCode != tc.code {
				t.Fatalf("got %d %s, want 400 %s", r.code, r.errCode, tc.code)
			}
			if n := m.auditCount(t, "request.validation_failed", r.requestID); n != 1 {
				t.Fatalf("request.validation_failed audit = %d, want 1", n)
			}
		})
	}

	var n int
	if err := m.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM payments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("payments stored from invalid requests = %d, %v", n, err)
	}
}
