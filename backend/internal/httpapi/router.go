// Package httpapi wires FinCore's HTTP routes, middleware and handlers.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
)

// Pool is the subset of *pgxpool.Pool the API needs.
type Pool interface {
	db.DBTX
	db.TxBeginner
	Ping(ctx context.Context) error
}

type Deps struct {
	DB Pool
}

// NewRouter builds the HTTP handler.
//
// Middleware order: requestID first so every log line and error carries
// it; accessLog outside recoverer so panics are logged with status 500.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, accessLog, recoverer)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed.")
	})

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.DB))

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(auth.Middleware(d.DB))
		r.Get("/me", me)
	})

	return r
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func readyz(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "NOT_READY", "Database is not reachable.")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// me returns the authenticated caller. Useful to verify a token.
func me(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	httpx.WriteJSON(w, http.StatusOK, id)
}
