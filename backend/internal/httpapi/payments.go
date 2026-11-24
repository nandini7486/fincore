package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/idempotency"
	"github.com/nandini7486/fincore/backend/internal/money"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
	"github.com/nandini7486/fincore/backend/internal/platform/ids"
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
	FailureReason     *string   `json:"failure_reason,omitempty"` // FAILED payments only
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
		FailureReason:     p.FailureReason,
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
// the caller owns to another user's account. Requires Idempotency-Key.
func createPayment(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())

		key, verr := idempotencyKey(r)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}
		var req createPaymentRequest
		if err := decodeJSONBody(w, r, &req, maxPaymentBodyBytes); err != nil {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_REQUEST",
				"Request body must be a JSON object with sender_account_id, receiver_account_id, amount and currency."})
			return
		}
		if !ids.IsUUID(req.SenderAccountID) || !ids.IsUUID(req.ReceiverAccountID) {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_ACCOUNT_ID",
				"sender_account_id and receiver_account_id must be account IDs."})
			return
		}
		if strings.EqualFold(req.SenderAccountID, req.ReceiverAccountID) {
			rejectInvalid(w, r, p, id.UserID, &validationError{"SAME_ACCOUNT",
				"Sender and receiver accounts must be different."})
			return
		}
		amount, verr := validateAmountAndCurrency(req.Amount, req.Currency)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}

		res, err := runMoneyRequest(r, p, moneyRequest{
			userID: id.UserID,
			key:    key,
			hash: idempotency.Hash("payment.create",
				idempotency.CanonicalID(req.SenderAccountID),
				idempotency.CanonicalID(req.ReceiverAccountID),
				strconv.FormatInt(amount, 10), req.Currency),
			run: func(tx pgx.Tx) (payments.Payment, error) {
				return payments.CreateTransfer(r.Context(), tx, payments.Request{
					SenderID:    req.SenderAccountID,
					ReceiverID:  req.ReceiverAccountID,
					Amount:      amount,
					Currency:    req.Currency,
					InitiatedBy: id.UserID,
				})
			},
		})
		switch {
		case errors.Is(err, payments.ErrSenderNotFound):
			// Same 404 as GET /accounts/{id}: never reveal someone else's
			// account. A real account owned by someone else is still audited.
			if errors.Is(err, payments.ErrSenderNotOwned) {
				auditDenied(r, p, "account", req.SenderAccountID)
			}
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Sender account not found.")
		case errors.Is(err, payments.ErrReceiverNotFound):
			httpx.WriteError(w, r, http.StatusUnprocessableEntity, "RECEIVER_NOT_FOUND", "Receiver account not found.")
		default:
			writeMoneyResult(w, r, p, key, res, err)
		}
	}
}

// createTopUp handles POST /api/v1/accounts/{accountID}/topups: the treasury
// funds a user account. Admin only (enforced by RequireRole in the router).
// Requires Idempotency-Key.
func createTopUp(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		accountID := chi.URLParam(r, "accountID")

		key, verr := idempotencyKey(r)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}
		var req createTopUpRequest
		if err := decodeJSONBody(w, r, &req, maxPaymentBodyBytes); err != nil {
			rejectInvalid(w, r, p, id.UserID, &validationError{"INVALID_REQUEST",
				"Request body must be a JSON object with amount and currency."})
			return
		}
		if !ids.IsUUID(accountID) {
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
			return
		}
		amount, verr := validateAmountAndCurrency(req.Amount, req.Currency)
		if verr != nil {
			rejectInvalid(w, r, p, id.UserID, verr)
			return
		}

		res, err := runMoneyRequest(r, p, moneyRequest{
			userID: id.UserID,
			key:    key,
			hash: idempotency.Hash("topup.create",
				idempotency.CanonicalID(accountID), strconv.FormatInt(amount, 10), req.Currency),
			run: func(tx pgx.Tx) (payments.Payment, error) {
				return payments.CreateTopUp(r.Context(), tx, payments.Request{
					ReceiverID:  accountID,
					Amount:      amount,
					Currency:    req.Currency,
					InitiatedBy: id.UserID,
				})
			},
		})
		if errors.Is(err, payments.ErrReceiverNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found.")
			return
		}
		writeMoneyResult(w, r, p, key, res, err)
	}
}

// idempotencyKey reads and validates the required Idempotency-Key header.
func idempotencyKey(r *http.Request) (string, *validationError) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return "", &validationError{"IDEMPOTENCY_KEY_REQUIRED",
			"An Idempotency-Key header is required. Send a new UUID per payment and reuse it on retries."}
	}
	if !idempotency.ValidKey(key) {
		return "", &validationError{"INVALID_IDEMPOTENCY_KEY",
			"Idempotency-Key must be 1-255 printable ASCII characters."}
	}
	return key, nil
}

type moneyRequest struct {
	userID, key string
	hash        []byte
	run         func(pgx.Tx) (payments.Payment, error)
}

// moneyResult is the response to send: freshly produced, or replayed.
type moneyResult struct {
	status   int
	body     []byte
	replayed bool
}

var (
	errReplay              = errors.New("idempotent replay")
	errIdempotencyConflict = errors.New("idempotency key reused with a different request")
)

// runMoneyRequest runs one money movement in a single transaction
// (Blueprint §6.1):
//
//  1. Set lock and statement timeouts.
//  2. Claim the Idempotency-Key. A completed identical request is replayed;
//     a different request with the same key is a conflict. Either way the
//     transaction rolls back without writing anything.
//  3. Move the money. A business-rule failure still commits: the FAILED
//     payment and its 422 response must be stored (§6.4).
//  4. Store the exact response with the key, then commit.
//
// Any other error rolls everything back, including the claim, so a retry
// with the same key is processed afresh.
func runMoneyRequest(r *http.Request, p Pool, m moneyRequest) (moneyResult, error) {
	ctx := r.Context()
	var res moneyResult
	err := db.WithTx(ctx, p, func(tx pgx.Tx) error {
		if err := payments.ApplyTimeouts(ctx, tx); err != nil {
			return err
		}
		outcome, stored, err := idempotency.Claim(ctx, tx, m.userID, m.key, m.hash)
		if err != nil {
			return payments.MapDBError(err)
		}
		switch outcome {
		case idempotency.Replay:
			res = moneyResult{status: stored.Status, body: stored.Body, replayed: true}
			return errReplay
		case idempotency.Conflict:
			return errIdempotencyConflict
		}

		pay, err := m.run(tx)
		var ruleErr *payments.RuleError
		switch {
		case errors.As(err, &ruleErr):
			pay = ruleErr.Payment
			reason := *pay.FailureReason
			res.status = http.StatusUnprocessableEntity
			res.body, err = json.Marshal(httpx.ErrorBody{Error: httpx.ErrorDetail{
				Code:      reason,
				Message:   ruleMessages[reason],
				RequestID: reqctx.RequestID(ctx),
				PaymentID: pay.ID,
			}})
		case err != nil:
			return err
		default:
			res.status = http.StatusCreated
			res.body, err = json.Marshal(toPaymentResponse(pay))
		}
		if err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		if err := idempotency.Save(ctx, tx, m.userID, m.key, pay.ID, res.status, res.body); err != nil {
			return payments.MapDBError(err)
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return res, nil
	}
	return res, err
}

func writeMoneyResult(w http.ResponseWriter, r *http.Request, p Pool, key string, res moneyResult, err error) {
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		if res.replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.WriteHeader(res.status)
		_, _ = w.Write(append(res.body, '\n'))
	case errors.Is(err, errIdempotencyConflict):
		id, _ := auth.FromContext(r.Context())
		actor := id.UserID
		audit.WriteBestEffort(r.Context(), p, audit.Entry{
			ActorUserID: &actor,
			Action:      "idempotency.conflict",
			Outcome:     audit.Failure,
			IPAddress:   httpx.ClientIP(r),
			Details:     map[string]any{"idempotency_key": key, "method": r.Method, "path": r.URL.Path},
		})
		httpx.WriteError(w, r, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED",
			"This Idempotency-Key was already used for a different request. Use a new key for a new payment.")
	case errors.Is(err, payments.ErrBusy):
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_BUSY",
			"The system is busy. Nothing was charged; please retry with the same Idempotency-Key.")
	default:
		httpx.WriteInternalError(w, r, err)
	}
}
