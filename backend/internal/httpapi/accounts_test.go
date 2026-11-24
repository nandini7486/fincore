package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/auth"
)

type accountBody struct {
	AccountID string `json:"account_id"`
	Type      string `json:"type"`
	Currency  string `json:"currency"`
	Balance   string `json:"balance"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (e *env) doBody(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) createAccount(t *testing.T, token string) accountBody {
	t.Helper()
	rec := e.doBody(t, http.MethodPost, "/api/v1/accounts", token, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account = %d: %s", rec.Code, rec.Body)
	}
	var a accountBody
	if err := json.NewDecoder(rec.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) auditCount(t *testing.T, action, requestID string) int {
	t.Helper()
	var n int
	err := e.db.Owner.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1 AND request_id = $2`, action, requestID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAccount(t *testing.T) {
	e := setup(t)
	_, token := e.createUser(t, "alice@example.com", auth.RoleUser)

	for _, tc := range []struct{ name, body string }{
		{"empty body", ""},
		{"explicit INR", `{"currency":"INR"}`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.doBody(t, http.MethodPost, "/api/v1/accounts", token, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			var a accountBody
			if err := json.NewDecoder(rec.Body).Decode(&a); err != nil {
				t.Fatal(err)
			}
			want := accountBody{AccountID: a.AccountID, Type: "USER", Currency: "INR", Balance: "0.00",
				Status: "ACTIVE", CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
			if a != want || a.AccountID == "" || a.CreatedAt == "" {
				t.Fatalf("account = %+v", a)
			}
			if loc := rec.Header().Get("Location"); loc != "/api/v1/accounts/"+a.AccountID {
				t.Fatalf("Location = %q", loc)
			}
			if n := e.auditCount(t, "account.create", rec.Header().Get("X-Request-ID")); n != 1 {
				t.Fatalf("account.create audit records = %d, want 1", n)
			}
		})
	}
}

func TestCreateAccountRejectsBadInput(t *testing.T) {
	e := setup(t)
	alice, token := e.createUser(t, "alice@example.com", auth.RoleUser)

	for _, tc := range []struct{ name, body, code string }{
		{"unsupported currency", `{"currency":"USD"}`, "UNSUPPORTED_CURRENCY"},
		{"client-chosen balance", `{"currency":"INR","balance":"1000000.00"}`, "INVALID_REQUEST"},
		{"malformed JSON", `{"currency":`, "INVALID_REQUEST"},
		{"not an object", `["INR"]`, "INVALID_REQUEST"},
		{"two objects", `{} {}`, "INVALID_REQUEST"},
		{"oversized body", `{"currency":"` + strings.Repeat("A", 2048) + `"}`, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.doBody(t, http.MethodPost, "/api/v1/accounts", token, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			if got := decodeError(t, rec).Code; got != tc.code {
				t.Fatalf("code = %s, want %s", got, tc.code)
			}
		})
	}

	var n int
	if err := e.db.Owner.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE user_id = $1`, alice.UserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("accounts created from bad input = %d, want 0", n)
	}
}

func TestAccountsRequireAuthentication(t *testing.T) {
	e := setup(t)
	if rec := e.doBody(t, http.MethodPost, "/api/v1/accounts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without token = %d", rec.Code)
	}
	path := "/api/v1/accounts/00000000-0000-4000-8000-000000000000"
	if rec := e.do(t, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without token = %d", rec.Code)
	}
}

func TestGetAccount(t *testing.T) {
	e := setup(t)
	_, aliceToken := e.createUser(t, "alice@example.com", auth.RoleUser)
	_, bobToken := e.createUser(t, "bob@example.com", auth.RoleUser)
	created := e.createAccount(t, aliceToken)

	t.Run("owner", func(t *testing.T) {
		rec := e.do(t, http.MethodGet, "/api/v1/accounts/"+created.AccountID, aliceToken, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var got accountBody
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != created {
			t.Fatalf("got %+v, want %+v", got, created)
		}
	})

	var treasury string
	if err := e.db.Owner.QueryRow(context.Background(),
		`SELECT id FROM accounts WHERE type = 'TREASURY'`).Scan(&treasury); err != nil {
		t.Fatal(err)
	}

	// Every way of not being allowed to see an account looks identical to
	// the client. Only an existing account owned by someone else is audited.
	for _, tc := range []struct {
		name, token, id string
		audited         bool
	}{
		{"another user's account", bobToken, created.AccountID, true},
		{"treasury account", aliceToken, treasury, true},
		{"missing account", aliceToken, "00000000-0000-4000-8000-000000000000", false},
		{"malformed id", aliceToken, "not-a-uuid", false},
		{"sql-ish id", aliceToken, "1' OR '1'='1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, http.MethodGet, "/api/v1/accounts/"+strings.ReplaceAll(tc.id, " ", "%20"), tc.token, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			errBody := decodeError(t, rec)
			if errBody.Code != "ACCOUNT_NOT_FOUND" || errBody.Message != "Account not found." {
				t.Fatalf("error = %+v", errBody)
			}
			want := 0
			if tc.audited {
				want = 1
			}
			if n := e.auditCount(t, "authz.denied", errBody.RequestID); n != want {
				t.Fatalf("authz.denied audit records = %d, want %d", n, want)
			}
		})
	}
}
