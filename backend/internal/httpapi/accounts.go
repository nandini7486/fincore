package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/money"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
)

// maxAccountBodyBytes caps the create-account request body. The only field
// is a currency code, so anything larger is not a legitimate request.
const maxAccountBodyBytes = 1 << 10

// accountResponse is the public JSON shape of an account. Money is a decimal
// string, never a JSON number, so clients cannot lose precision.
type accountResponse struct {
	AccountID string    `json:"account_id"`
	Type      string    `json:"type"`
	Currency  string    `json:"currency"`
	Balance   string    `json:"balance"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toAccountResponse(a accounts.Account) accountResponse {
	return accountResponse{
		AccountID: a.ID,
		Type:      a.Type,
		Currency:  a.Currency,
		Balance:   money.Format(a.Balance),
		Status:    a.Status,
		CreatedAt: a.CreatedAt.UTC(),
		UpdatedAt: a.UpdatedAt.UTC(),
	}
}

type createAccountRequest struct {
	Currency string `json:"currency"`
}

// createAccount handles POST /api/v1/accounts. The body is optional; when
// present it may only contain "currency".
func createAccount(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())

		var req createAccountRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAccountBodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpx.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Request body must be a JSON object with an optional \"currency\" field.")
			return
		}
		if dec.More() {
			httpx.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Request body must contain a single JSON object.")
			return
		}

		var acct accounts.Account
		err := db.WithTx(r.Context(), p, func(tx pgx.Tx) error {
			var err error
			acct, err = accounts.Create(r.Context(), tx, id.UserID, req.Currency)
			return err
		})
		if errors.Is(err, accounts.ErrInvalidInput) {
			httpx.WriteError(w, r, http.StatusBadRequest, "UNSUPPORTED_CURRENCY", "Only INR accounts are supported.")
			return
		}
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}

		w.Header().Set("Location", "/api/v1/accounts/"+acct.ID)
		httpx.WriteJSON(w, http.StatusCreated, toAccountResponse(acct))
	}
}

// getAccount handles GET /api/v1/accounts/{accountID}. Only the owner may
// see an account. A malformed ID, a missing account and someone else's
// account all get the same 404, so callers cannot discover which account
// IDs exist (see docs/decisions/0001).
func getAccount(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		accountID := chi.URLParam(r, "accountID")

		notFound := func() {
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
		}
		if !isUUID(accountID) {
			notFound()
			return
		}

		acct, err := accounts.GetForOwner(r.Context(), p, accountID, id.UserID)
		switch {
		case errors.Is(err, accounts.ErrNotFound):
			notFound()
		case errors.Is(err, accounts.ErrNotOwner):
			// The account exists but is not the caller's: worth a security
			// record, but the client sees exactly the same 404.
			actor := id.UserID
			audit.WriteBestEffort(r.Context(), p, audit.Entry{
				ActorUserID:  &actor,
				Action:       "authz.denied",
				ResourceType: "account",
				ResourceID:   accountID,
				Outcome:      audit.Denied,
				IPAddress:    httpx.ClientIP(r),
				Details:      map[string]any{"method": r.Method, "path": r.URL.Path},
			})
			notFound()
		case err != nil:
			httpx.WriteInternalError(w, r, err)
		default:
			httpx.WriteJSON(w, http.StatusOK, toAccountResponse(acct))
		}
	}
}

// isUUID reports whether s is a canonical UUID (8-4-4-4-12 hex digits).
// Checking before querying keeps malformed IDs from reaching PostgreSQL,
// which would otherwise reject them with an error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
