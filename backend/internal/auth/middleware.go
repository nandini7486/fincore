package auth

import (
	"errors"
	"net/http"

	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
)

// Middleware authenticates the bearer token and stores the Identity in the
// request context. Failures return 401 and are audited (best effort).
func Middleware(q db.DBTX) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthenticated(w, r, q, "missing_token")
				return
			}
			id, err := LookupByToken(r.Context(), q, token)
			if errors.Is(err, ErrUnknownToken) {
				unauthenticated(w, r, q, "invalid_token")
				return
			}
			if err != nil {
				httpx.WriteInternalError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

func unauthenticated(w http.ResponseWriter, r *http.Request, q db.DBTX, reason string) {
	audit.WriteBestEffort(r.Context(), q, audit.Entry{
		Action:    "auth.failure",
		Outcome:   audit.Failure,
		IPAddress: httpx.ClientIP(r),
		Details:   map[string]any{"reason": reason, "method": r.Method, "path": r.URL.Path},
	})
	w.Header().Set("WWW-Authenticate", `Bearer realm="fincore"`)
	httpx.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Missing or invalid bearer token.")
}

// RequireRole allows only callers with the given role. Others get 403 and
// an authz.denied audit record. Must run after Middleware.
func RequireRole(q db.DBTX, role Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				httpx.WriteInternalError(w, r, errors.New("RequireRole used without auth.Middleware"))
				return
			}
			if id.Role != role {
				actor := id.UserID
				audit.WriteBestEffort(r.Context(), q, audit.Entry{
					ActorUserID: &actor,
					Action:      "authz.denied",
					Outcome:     audit.Denied,
					IPAddress:   httpx.ClientIP(r),
					Details:     map[string]any{"required_role": string(role), "method": r.Method, "path": r.URL.Path},
				})
				httpx.WriteError(w, r, http.StatusForbidden, "FORBIDDEN", "You do not have permission to perform this action.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
