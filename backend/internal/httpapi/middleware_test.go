package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests need no database: /healthz and middleware never touch it.

func TestValidRequestID(t *testing.T) {
	valid := []string{"a", "req_123", "client-abc.123", strings.Repeat("x", 128)}
	invalid := []string{"", "has space", "new\nline", "quote\"", strings.Repeat("x", 129), "émoji"}
	for _, id := range valid {
		if !validRequestID(id) {
			t.Errorf("validRequestID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if validRequestID(id) {
			t.Errorf("validRequestID(%q) = true, want false", id)
		}
	}
}

func TestHealthzWithoutDatabase(t *testing.T) {
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("X-Request-ID"), "req_") {
		t.Fatal("missing generated X-Request-ID")
	}
}

func TestRecovererReturns500(t *testing.T) {
	h := requestID(recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("panic value leaked to client")
	}
}
