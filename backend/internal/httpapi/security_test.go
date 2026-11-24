package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/httpapi"
)

// publicRoutes are the only routes allowed without a bearer token.
// Adding a route here is a security decision: it needs review.
var publicRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /readyz":  true,
}

// adminRoutes must reject authenticated non-admins with 403.
var adminRoutes = map[string]bool{
	"POST /api/v1/accounts/{accountID}/topups": true,
}

type route struct{ method, pattern string }

// allRoutes lists every route registered on the real router, so a new
// endpoint is covered by these tests automatically.
func allRoutes(t *testing.T, h http.Handler) []route {
	t.Helper()
	mux, ok := h.(chi.Routes)
	if !ok {
		t.Fatal("router is not a chi.Routes; cannot enumerate routes")
	}
	var routes []route
	err := chi.Walk(mux, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, route{method, pattern})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) < 5 {
		t.Fatalf("found only %d routes; route walking is broken", len(routes))
	}
	return routes
}

// concrete fills path parameters with a well-formed ID.
func concrete(pattern string) string {
	for {
		i := strings.Index(pattern, "{")
		if i < 0 {
			return pattern
		}
		j := strings.Index(pattern[i:], "}")
		pattern = pattern[:i] + "00000000-0000-4000-8000-000000000000" + pattern[i+j+1:]
	}
}

// Every route except the public allowlist must reject requests without a
// valid bearer token, before any handler logic runs.
func TestEveryRouteRequiresAuthentication(t *testing.T) {
	e := setup(t)
	_, validToken := e.createUser(t, "alice@example.com", auth.RoleUser)

	seenPublic := map[string]bool{}
	for _, rt := range allRoutes(t, httpapi.NewRouter(httpapi.Deps{DB: e.db.App})) {
		name := rt.method + " " + rt.pattern
		if publicRoutes[name] {
			seenPublic[name] = true
			continue
		}
		if !strings.HasPrefix(rt.pattern, "/api/v1/") {
			t.Errorf("%s is outside /api/v1 and not in publicRoutes: unauthenticated by design? Review it.", name)
			continue
		}
		path := concrete(rt.pattern)

		for _, tc := range []struct{ desc, header string }{
			{"no Authorization header", ""},
			{"empty bearer token", "Bearer "},
			{"wrong scheme", "Basic " + validToken},
			{"token without scheme", validToken},
			{"unknown token", "Bearer fct_" + strings.Repeat("A", 43)},
			{"valid token with a character changed", "Bearer " + validToken[:len(validToken)-1] + flip(validToken[len(validToken)-1])},
		} {
			t.Run(name+"/"+tc.desc, func(t *testing.T) {
				req := httptest.NewRequest(rt.method, path, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", "security-test")
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()
				e.handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rec.Code)
				}
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("401 without WWW-Authenticate header")
				}
				if strings.Contains(rec.Body.String(), "fct_") {
					t.Fatalf("response echoes a token: %s", rec.Body)
				}
			})
		}
	}
	for name := range publicRoutes {
		if !seenPublic[name] {
			t.Errorf("publicRoutes lists %s, which no longer exists; remove it", name)
		}
	}
}

// Admin-only routes reject authenticated non-admins with 403.
func TestAdminRoutesRejectNonAdmins(t *testing.T) {
	e := setup(t)
	_, userToken := e.createUser(t, "alice@example.com", auth.RoleUser)

	found := map[string]bool{}
	for _, rt := range allRoutes(t, httpapi.NewRouter(httpapi.Deps{DB: e.db.App})) {
		name := rt.method + " " + rt.pattern
		if !adminRoutes[name] {
			continue
		}
		found[name] = true
		req := httptest.NewRequest(rt.method, concrete(rt.pattern), strings.NewReader(`{"amount":"1.00","currency":"INR"}`))
		req.Header.Set("Authorization", "Bearer "+userToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "security-test")
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s as a normal user = %d, want 403", name, rec.Code)
		}
	}
	for name := range adminRoutes {
		if !found[name] {
			t.Errorf("adminRoutes lists %s, which no longer exists", name)
		}
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}
