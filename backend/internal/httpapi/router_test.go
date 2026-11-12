package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/httpapi"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"

	"github.com/jackc/pgx/v5"
)

type env struct {
	db      *testdb.DB
	handler http.Handler
}

func setup(t *testing.T) *env {
	t.Helper()
	tdb := testdb.New(t)
	return &env{db: tdb, handler: httpapi.NewRouter(httpapi.Deps{DB: tdb.App})}
}

func (e *env) do(t *testing.T, method, path, token string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) createUser(t *testing.T, email string, role auth.Role) (auth.Identity, string) {
	t.Helper()
	var id auth.Identity
	var token string
	err := db.WithTx(context.Background(), e.db.Owner, func(tx pgx.Tx) error {
		var err error
		id, token, err = users.Create(context.Background(), tx, users.NewUser{Name: "Test", Email: email, Role: role})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, token
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorDetail {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return body.Error
}

func TestHealthAndReadiness(t *testing.T) {
	e := setup(t)

	if rec := e.do(t, http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/readyz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d", rec.Code)
	}

	rec := e.do(t, http.MethodPost, "/healthz", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz = %d", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "METHOD_NOT_ALLOWED" {
		t.Fatalf("code = %s", got)
	}

	rec = e.do(t, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "NOT_FOUND" {
		t.Fatalf("unknown route = %d", rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	e := setup(t)

	rec := e.do(t, http.MethodGet, "/healthz", "", map[string]string{"X-Request-ID": "client-abc.123"})
	if got := rec.Header().Get("X-Request-ID"); got != "client-abc.123" {
		t.Fatalf("echoed request id = %q", got)
	}

	rec = e.do(t, http.MethodGet, "/healthz", "", map[string]string{"X-Request-ID": "bad id\nwith newline"})
	if got := rec.Header().Get("X-Request-ID"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("invalid client id should be replaced, got %q", got)
	}
}

func TestAuthentication(t *testing.T) {
	e := setup(t)
	alice, token := e.createUser(t, "alice@example.com", auth.RoleUser)

	t.Run("valid token", func(t *testing.T) {
		rec := e.do(t, http.MethodGet, "/api/v1/me", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var got auth.Identity
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != alice {
			t.Fatalf("identity = %+v, want %+v", got, alice)
		}
	})

	for _, tc := range []struct {
		name, header, reason string
	}{
		{"missing token", "", "missing_token"},
		{"invalid token", "Bearer fct_not_a_real_token", "invalid_token"},
		{"wrong scheme", "Basic " + token, "missing_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Authorization"] = tc.header
			}
			rec := e.do(t, http.MethodGet, "/api/v1/me", "", hdr)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate header")
			}
			errBody := decodeError(t, rec)
			if errBody.Code != "UNAUTHENTICATED" {
				t.Fatalf("code = %s", errBody.Code)
			}
			if errBody.RequestID == "" || errBody.RequestID != rec.Header().Get("X-Request-ID") {
				t.Fatalf("request_id %q does not match header", errBody.RequestID)
			}

			// The failure is audited under the same request ID, without the token.
			var reason, details string
			err := e.db.Owner.QueryRow(context.Background(), `
				SELECT details->>'reason', details::text FROM audit_log
				WHERE action = 'auth.failure' AND outcome = 'FAILURE' AND request_id = $1`,
				errBody.RequestID).Scan(&reason, &details)
			if err != nil {
				t.Fatalf("audit record: %v", err)
			}
			if reason != tc.reason {
				t.Fatalf("audit reason = %q, want %q", reason, tc.reason)
			}
			if strings.Contains(details, "fct_") {
				t.Fatalf("audit details leaked a token: %s", details)
			}
		})
	}
}
