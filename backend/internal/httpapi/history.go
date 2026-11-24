package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
	"github.com/nandini7486/fincore/backend/internal/platform/ids"
)

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
)

// getPayment handles GET /api/v1/payments/{paymentID} (FR-06). Visible to
// the owner of the sender or receiver account; everyone else gets the same
// 404 as for a payment that does not exist.
func getPayment(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		paymentID := chi.URLParam(r, "paymentID")

		notFound := func() {
			httpx.WriteError(w, r, http.StatusNotFound, "PAYMENT_NOT_FOUND", "Payment not found.")
		}
		if !ids.IsUUID(paymentID) {
			notFound()
			return
		}

		pay, err := payments.GetForUser(r.Context(), p, paymentID, id.UserID)
		switch {
		case errors.Is(err, payments.ErrPaymentNotFound):
			notFound()
		case errors.Is(err, payments.ErrPaymentNotVisible):
			auditDenied(r, p, "payment", paymentID)
			notFound()
		case err != nil:
			httpx.WriteInternalError(w, r, err)
		default:
			httpx.WriteJSON(w, http.StatusOK, toPaymentResponse(pay))
		}
	}
}

type historyResponse struct {
	Payments []paymentResponse `json:"payments"`
	// NextCursor is null on the last page. Pass it back as ?cursor= to get
	// the next page.
	NextCursor *string `json:"next_cursor"`
}

// accountPayments handles GET /api/v1/accounts/{accountID}/payments (FR-07):
// payments sent or received by the account, newest first, with keyset
// pagination. Owner only.
func accountPayments(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := defaultHistoryLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxHistoryLimit {
				httpx.WriteError(w, r, http.StatusBadRequest, "INVALID_LIMIT", "limit must be a whole number from 1 to 100.")
				return
			}
			limit = n
		}
		var after *payments.Cursor
		if v := r.URL.Query().Get("cursor"); v != "" {
			c, err := payments.DecodeCursor(v)
			if err != nil {
				httpx.WriteError(w, r, http.StatusBadRequest, "INVALID_CURSOR", "cursor is not valid. Use the next_cursor value from a previous page.")
				return
			}
			after = &c
		}

		acct, ok := ownedAccount(w, r, p)
		if !ok {
			return
		}

		page, next, err := payments.History(r.Context(), p, acct.ID, after, limit)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		resp := historyResponse{Payments: make([]paymentResponse, 0, len(page))}
		for _, pay := range page {
			resp.Payments = append(resp.Payments, toPaymentResponse(pay))
		}
		if next != nil {
			s := next.Encode()
			resp.NextCursor = &s
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}
