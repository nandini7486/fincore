// Package payments moves money between accounts: user-to-user transfers
// (FR-01 to FR-05) and treasury top-ups (FR-10).
//
// Every money movement follows one path, inside one database transaction
// (Blueprint §6.1, NFR-02, NFR-03):
//
//  1. Lock both account rows in one statement, in ID order.
//  2. Check the business rules while the locks are held.
//  3. On a rule failure, store a FAILED payment and change nothing else.
//  4. Otherwise store a SUCCESS payment, debit and credit the balances, and
//     write one DEBIT and one CREDIT ledger entry.
//  5. Write the payment.create / topup.create audit record.
//
// The caller commits, so all of it becomes visible together or not at all.
// Idempotency-Key handling is added in Phase 3.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nandini7486/fincore/backend/internal/audit"
	"github.com/nandini7486/fincore/backend/internal/platform/reqctx"
)

type Type string

const (
	Transfer Type = "TRANSFER"
	TopUp    Type = "TOPUP"
)

// Failure reasons stored on FAILED payments (FR-08).
const (
	ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"
	ReasonAccountInactive   = "ACCOUNT_INACTIVE"
	ReasonCurrencyMismatch  = "CURRENCY_MISMATCH"
)

var (
	// ErrSenderNotFound: the sender does not exist or the caller does not
	// own it. Respond 404; nothing is stored.
	ErrSenderNotFound = errors.New("sender account not found")
	// ErrReceiverNotFound: the receiver does not exist or cannot receive
	// payments (the treasury). Nothing is stored: a payment row must
	// reference real accounts.
	ErrReceiverNotFound = errors.New("receiver account not found")
	// ErrBusy: a lock or statement timeout. Nothing was committed and the
	// client may safely retry.
	ErrBusy = errors.New("database busy")
)

// ErrSenderNotOwned is the case of ErrSenderNotFound where the account
// exists but the caller may not spend from it. errors.Is(err,
// ErrSenderNotFound) is true for it, so clients get the same 404; the API
// additionally audits it as authz.denied (ADR 0001).
var ErrSenderNotOwned = fmt.Errorf("%w: owned by another user", ErrSenderNotFound)

// RuleError is a business-rule failure (FR-08). The FAILED payment has been
// written to the transaction, which the caller should still commit.
type RuleError struct {
	Payment Payment
}

func (e *RuleError) Error() string { return "payment failed: " + *e.Payment.FailureReason }

// Request describes one money movement. Amount is in minor units and has
// already been validated by money.Parse.
type Request struct {
	Type        Type
	SenderID    string
	ReceiverID  string
	Amount      int64
	Currency    string
	InitiatedBy string // user ID from the bearer token, never from the body
}

type Payment struct {
	ID            string
	Type          Type
	SenderID      string
	ReceiverID    string
	Amount        int64
	Currency      string
	Status        string
	FailureReason *string
	CreatedAt     time.Time
}

// Timeouts for money-moving transactions (NFR-02). A request that cannot
// get its locks quickly fails with ErrBusy instead of queueing forever.
const (
	lockTimeout      = "2s"
	statementTimeout = "5s"
)

type lockedAccount struct {
	id       string
	userID   *string
	typ      string
	currency string
	balance  int64
	status   string
}

// CreateTransfer moves money from the caller's account to another user's
// account. It must run inside a transaction.
func CreateTransfer(ctx context.Context, tx pgx.Tx, r Request) (Payment, error) {
	r.Type = Transfer
	return execute(ctx, tx, r, func(sender lockedAccount) bool {
		return sender.typ == "USER" && sender.userID != nil && *sender.userID == r.InitiatedBy
	})
}

// CreateTopUp moves money from the treasury into a user account. Only
// admins may call it; the role check happens in the HTTP layer. It must run
// inside a transaction.
func CreateTopUp(ctx context.Context, tx pgx.Tx, r Request) (Payment, error) {
	r.Type = TopUp
	err := tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE type = 'TREASURY' AND currency = $1`, r.Currency,
	).Scan(&r.SenderID)
	if err != nil {
		return Payment{}, mapDBError(fmt.Errorf("find treasury: %w", err))
	}
	if r.ReceiverID == r.SenderID {
		return Payment{}, ErrReceiverNotFound // the treasury cannot top itself up
	}
	return execute(ctx, tx, r, func(sender lockedAccount) bool {
		return sender.typ == "TREASURY"
	})
}

func execute(ctx context.Context, tx pgx.Tx, r Request, senderAllowed func(lockedAccount) bool) (Payment, error) {
	if r.SenderID == r.ReceiverID {
		// The HTTP layer rejects this as a 400; the database also refuses it.
		return Payment{}, fmt.Errorf("sender and receiver must differ")
	}
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+lockTimeout+"'"); err != nil {
		return Payment{}, mapDBError(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
		return Payment{}, mapDBError(err)
	}

	// Lock both rows in ONE statement, ordered by ID. Two opposite
	// transfers (A->B and B->A) therefore always lock in the same order and
	// cannot deadlock. Checks below run while the locks are held, so no
	// concurrent payment can change these balances in between.
	rows, err := tx.Query(ctx, `
		SELECT id, user_id, type, currency, balance, status
		FROM accounts
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR UPDATE`,
		[]string{r.SenderID, r.ReceiverID})
	if err != nil {
		return Payment{}, mapDBError(fmt.Errorf("lock accounts: %w", err))
	}
	locked := map[string]lockedAccount{}
	for rows.Next() {
		var a lockedAccount
		if err := rows.Scan(&a.id, &a.userID, &a.typ, &a.currency, &a.balance, &a.status); err != nil {
			rows.Close()
			return Payment{}, mapDBError(fmt.Errorf("scan account: %w", err))
		}
		locked[a.id] = a
	}
	if err := rows.Err(); err != nil {
		return Payment{}, mapDBError(fmt.Errorf("lock accounts: %w", err))
	}

	sender, ok := locked[r.SenderID]
	if !ok {
		return Payment{}, ErrSenderNotFound
	}
	if !senderAllowed(sender) {
		return Payment{}, ErrSenderNotOwned
	}
	receiver, ok := locked[r.ReceiverID]
	if !ok || receiver.typ != "USER" {
		return Payment{}, ErrReceiverNotFound
	}

	// Business rules (FR-02, FR-03). Order matters only for which reason is
	// reported when several apply.
	var reason string
	switch {
	case sender.status != "ACTIVE" || receiver.status != "ACTIVE":
		reason = ReasonAccountInactive
	case sender.currency != r.Currency || receiver.currency != r.Currency:
		reason = ReasonCurrencyMismatch
	case sender.typ == "USER" && sender.balance < r.Amount:
		// The treasury may go negative: its balance is the money issued.
		reason = ReasonInsufficientFunds
	}

	p, err := insertPayment(ctx, tx, r, reason)
	if err != nil {
		return Payment{}, err
	}

	if reason == "" {
		if err := move(ctx, tx, p); err != nil {
			return Payment{}, err
		}
	}

	if err := writeAudit(ctx, tx, r, p); err != nil {
		return Payment{}, mapDBError(err)
	}
	if reason != "" {
		return p, &RuleError{Payment: p}
	}
	return p, nil
}

func insertPayment(ctx context.Context, tx pgx.Tx, r Request, reason string) (Payment, error) {
	status := "SUCCESS"
	var failure *string
	if reason != "" {
		status = "FAILED"
		failure = &reason
	}
	p := Payment{Type: r.Type, SenderID: r.SenderID, ReceiverID: r.ReceiverID,
		Amount: r.Amount, Currency: r.Currency, Status: status, FailureReason: failure}
	err := tx.QueryRow(ctx, `
		INSERT INTO payments
		    (type, sender_account_id, receiver_account_id, amount, currency,
		     status, failure_reason, initiated_by_user_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		string(r.Type), r.SenderID, r.ReceiverID, r.Amount, r.Currency,
		status, failure, r.InitiatedBy, reqctx.RequestID(ctx),
	).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return Payment{}, mapDBError(fmt.Errorf("insert payment: %w", err))
	}
	return p, nil
}

// move applies a successful payment: both balance changes and both ledger
// entries. Debits always equal credits, so the global ledger sum stays zero.
func move(ctx context.Context, tx pgx.Tx, p Payment) error {
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET balance = balance - $2, updated_at = now() WHERE id = $1`,
		p.SenderID, p.Amount); err != nil {
		return mapDBError(fmt.Errorf("debit sender: %w", err))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET balance = balance + $2, updated_at = now() WHERE id = $1`,
		p.ReceiverID, p.Amount); err != nil {
		return mapDBError(fmt.Errorf("credit receiver: %w", err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
		VALUES ($1, $2, 'DEBIT', $4, $5), ($1, $3, 'CREDIT', $4, $5)`,
		p.ID, p.SenderID, p.ReceiverID, p.Amount, p.Currency); err != nil {
		return mapDBError(fmt.Errorf("insert ledger entries: %w", err))
	}
	return nil
}

func writeAudit(ctx context.Context, tx pgx.Tx, r Request, p Payment) error {
	action := "payment.create"
	if r.Type == TopUp {
		action = "topup.create"
	}
	outcome := audit.Success
	details := map[string]any{
		"type":                string(p.Type),
		"sender_account_id":   p.SenderID,
		"receiver_account_id": p.ReceiverID,
		"amount_minor":        p.Amount,
		"currency":            p.Currency,
		"status":              p.Status,
	}
	if p.FailureReason != nil {
		outcome = audit.Failure
		details["failure_reason"] = *p.FailureReason
	}
	actor := r.InitiatedBy
	return audit.Write(ctx, tx, audit.Entry{
		ActorUserID:  &actor,
		Action:       action,
		ResourceType: "payment",
		ResourceID:   p.ID,
		Outcome:      outcome,
		Details:      details,
	})
}

// mapDBError turns lock and statement timeouts into ErrBusy so the API can
// answer 503 (safe to retry) instead of 500.
func mapDBError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03", // lock_not_available (lock_timeout)
			"57014": // query_canceled (statement_timeout)
			return fmt.Errorf("%w: %v", ErrBusy, err)
		}
	}
	return err
}
