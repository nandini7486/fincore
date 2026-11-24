package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/money"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
	"github.com/nandini7486/fincore/backend/internal/platform/reqctx"
)

const maxPaymentBodyBytes = 4 << 10

// paymentResponse is the public JSON shape of a payment (Blueprint §7).
type paymentResponse struct {
	PaymentID         string    `json:"payment_id"`
	Type              string    `json:"type"`
	SenderAccountID   string    `json:"sender_account_id"`
	ReceiverAccountID string    `json:"receiver_account_id"`
	Amount            string    `json:"amount"`
	Currency          string    `json:"currency"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

func toPaymentResponse(p payments.Payment) paymentResponse {
	return paymentResponse{
		PaymentID:         p.ID,
		Type:              string(p.Type),
		SenderAccountID:   p.SenderID,
		ReceiverAccountID: p.ReceiverID,
		Amount:            money.Format(p.Amount),
		Currency:          p.Currency,
		Status:            p.Status,
		CreatedAt:         p.CreatedAt.UTC(),
	}
}

var ruleMessages = map[string]string{
	payments.ReasonInsufficientFunds: "Sender account has insufficient funds.",
	payments.ReasonAccountInactive:   "Sender or receiver account is not active.",
	payments.ReasonCurrencyMismatch:  "Account currency does not match the payment currency.",
}

type createPaymentRequest struct {
	SenderAccountID   string `json:"sender_account_id"`
	ReceiverAccountID string `json:"receiver_account_id"`
	Amount            string `json:"amount"`
	Currency          string `json:"currency"`
}

type createTopUpRequest struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// validationError is a 400 with a specific code. Money endpoints audit
// these as request.validation_failed (Blueprint §8.3).
type validationError struct {
	code, message string
}

func validateAmountAndCurrency(amount, currency string) (int64, *validationError) {
	minor, err := money.Parse(amount)
	if err != nil {
		return 0, &validationError{"INVALID_AMOUNT",
			"Amount must be a decimal string greater than 0 with at most 2 decimal places, up to " +
				money.Format(money.MaxAmount) + "."}
	}
	if currency != accounts.SupportedCurrency {
		return 0, &validationError{"UNSUPPORTED_CURRENCY", "Only INR payments are supported."}
	}
	return minor, nil
}

func rejectInvalid(w http.ResponseWriter, r *http.Request, p Pool, actor string, v *validationError) {
	audit.WriteBestEffort(r.Context(), p, audit.Entry{
		ActorUserID: &actor,
		Action:      "request.validation_failed",
		Outcome:     audit.Failure,
		IPAddress:   httpx.ClientIP(r),
		Details:     map[string]any{"code": v.code, "method": r.Method, "path": r.URL.Path},
	})
	httpx.WriteError(w, r, http.StatusBadRequest, v.code, v.message)
}

// createPayment handles POST /api/v1/payments: a transfer from an account
// the caller owns to another user's account.
func createPayment(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())

		var req createPaymentRequest
		if err := decodeJSONBody(w, r, &req, maxPaymentBodyBytes); err != nil {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_REQUEST",
				"Request body must be a JSON object with sender_account_id, receiver_account_id, amount and currency."})
			return
		}
		if !isUUID(req.SenderAccountID) || !isUUID(req.ReceiverAccountID) {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_ACCOUNT_ID",
				"sender_account_id and receiver_account_id must be account IDs."})
			return
		}
		if req.SenderAccountID == req.ReceiverAccountID {
			rejectInvalid(w, r, p, id.UserID, &validationError{"SAME_ACCOUNT",
				"Sender and receiver accounts must be different."})
			return
		}
		amount, verr := validateAmountAndCurrency(req.Amount, req.Currency)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}

		pay, err := commitPayment(r, p, func(tx pgx.Tx) (payments.Payment, error) {
			return payments.CreateTransfer(r.Context(), tx, payments.Request{
				SenderID:    req.SenderAccountID,
				ReceiverID:  req.ReceiverAccountID,
				Amount:      amount,
				Currency:    req.Currency,
				InitiatedBy: id.UserID,
			})
		})
		switch {
		case errors.Is(err, payments.ErrSenderNotFound):
			// Same 404 as GET /accounts/{id}: never reveal someone else's
			// account. A real account owned by someone else is still audited.
			if errors.Is(err, payments.ErrSenderNotOwned) {
				actor := id.UserID
				audit.WriteBestEffort(r.Context(), p, audit.Entry{
					ActorUserID:  &actor,
					Action:       "authz.denied",
					ResourceType: "account",
					ResourceID:   req.SenderAccountID,
					Outcome:      audit.Denied,
					IPAddress:    httpx.ClientIP(r),
					Details:      map[string]any{"method": r.Method, "path": r.URL.Path},
				})
			}
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Sender account not found.")
		case errors.Is(err, payments.ErrReceiverNotFound):
			httpx.WriteError(w, r, http.StatusUnprocessableEntity, "RECEIVER_NOT_FOUND", "Receiver account not found.")
		default:
			writePaymentResult(w, r, pay, err)
		}
	}
}

// createTopUp handles POST /api/v1/accounts/{accountID}/topups: the treasury
// funds a user account. Admin only (enforced by RequireRole in the router).
func createTopUp(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		accountID := chi.URLParam(r, "accountID")

		var req createTopUpRequest
		if err := decodeJSONBody(w, r, &req, maxPaymentBodyBytes); err != nil {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_REQUEST",
				"Request body must be a JSON object with amount and currency."})
			return
		}
		if !isUUID(accountID) {
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
			return
		}
		amount, verr := validateAmountAndCurrency(req.Amount, req.Currency)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}

		pay, err := commitPayment(r, p, func(tx pgx.Tx) (payments.Payment, error) {
			return payments.CreateTopUp(r.Context(), tx, payments.Request{
				ReceiverID:  accountID,
				Amount:      amount,
				Currency:    req.Currency,
				InitiatedBy: id.UserID,
			})
		})
		if errors.Is(err, payments.ErrReceiverNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
			return
		}
		writePaymentResult(w, r, pay, err)
	}
}

// commitPayment runs fn in a transaction. A business-rule failure still
// commits, because the FAILED payment and its audit record must be stored
// (Blueprint §6.4); every other error rolls everything back.
func commitPayment(r *http.Request, p Pool, fn func(pgx.Tx) (payments.Payment, error)) (payments.Payment, error) {
	var pay payments.Payment
	var ruleErr *payments.RuleError
	err := db.WithTx(r.Context(), p, func(tx pgx.Tx) error {
		var err error
		pay, err = fn(tx)
		if errors.As(err, &ruleErr) {
			return nil // commit the FAILED payment
		}
		return err
	})
	if err == nil && ruleErr != nil {
		return ruleErr.Payment, ruleErr
	}
	return pay, err
}

func writePaymentResult(w http.ResponseWriter, r *http.Request, pay payments.Payment, err error) {
	var ruleErr *payments.RuleError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, toPaymentResponse(pay))
	case errors.As(err, &ruleErr):
		reason := *ruleErr.Payment.FailureReason
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: httpx.ErrorDetail{
			Code:      reason,
			Message:   ruleMessages[reason],
			RequestID: reqctx.RequestID(r.Context()),
			PaymentID: ruleErr.Payment.ID,
		}})
	case errors.Is(err, payments.ErrBusy):
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_BUSY",
			"The system is busy. Nothing was charged; please retry.")
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
