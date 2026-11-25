package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/platform/cursor"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

var (
	ErrPaymentNotFound = errors.New("payment not found")
	// ErrPaymentNotVisible: the payment exists, but the caller owns neither
	// side. Respond exactly as for ErrPaymentNotFound (ADR 0001).
	ErrPaymentNotVisible = errors.New("payment not visible to caller")
)

const paymentColumns = `id, type, sender_account_id, receiver_account_id, amount, currency,
	status, failure_reason, created_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.Type, &p.SenderID, &p.ReceiverID, &p.Amount, &p.Currency,
		&p.Status, &p.FailureReason, &p.CreatedAt)
	return p, err
}

// GetForUser returns a payment if userID owns its sender or receiver
// account (FR-06).
func GetForUser(ctx context.Context, q db.DBTX, paymentID, userID string) (Payment, error) {
	var p Payment
	var visible bool
	err := q.QueryRow(ctx, `
		SELECT p.id, p.type, p.sender_account_id, p.receiver_account_id, p.amount, p.currency,
		       p.status, p.failure_reason, p.created_at,
		       (s.user_id = $2 OR r.user_id = $2) IS TRUE
		FROM payments p
		JOIN accounts s ON s.id = p.sender_account_id
		JOIN accounts r ON r.id = p.receiver_account_id
		WHERE p.id = $1`,
		paymentID, userID,
	).Scan(&p.ID, &p.Type, &p.SenderID, &p.ReceiverID, &p.Amount, &p.Currency,
		&p.Status, &p.FailureReason, &p.CreatedAt, &visible)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	if !visible {
		return Payment{}, ErrPaymentNotVisible
	}
	return p, nil
}

// Cursor, DecodeCursor and ErrInvalidCursor are the shared keyset cursor
// (package cursor), kept here so payment callers read naturally.
type Cursor = cursor.Cursor

var ErrInvalidCursor = cursor.ErrInvalid

func DecodeCursor(s string) (Cursor, error) { return cursor.Decode(s) }

// History returns up to limit payments involving accountID (sent or
// received, any status), newest first, starting after cursor (nil for the
// first page). next is nil when there are no more payments.
//
// Ownership must be checked by the caller. Each half of the UNION ALL is
// served by its own index, (sender_account_id, created_at DESC, id DESC)
// and the receiver equivalent, so a page costs O(limit) index reads no
// matter how deep into the history it is (unlike OFFSET).
func History(ctx context.Context, q db.DBTX, accountID string, after *Cursor, limit int) (page []Payment, next *Cursor, err error) {
	if limit < 1 {
		return nil, nil, fmt.Errorf("limit must be positive")
	}
	// Fetch one extra row to learn whether another page exists.
	fetch := limit + 1

	var rows pgx.Rows
	if after == nil {
		rows, err = q.Query(ctx, `
			(SELECT `+paymentColumns+` FROM payments WHERE sender_account_id = $1
			 ORDER BY created_at DESC, id DESC LIMIT $2)
			UNION ALL
			(SELECT `+paymentColumns+` FROM payments WHERE receiver_account_id = $1
			 ORDER BY created_at DESC, id DESC LIMIT $2)
			ORDER BY created_at DESC, id DESC
			LIMIT $2`,
			accountID, fetch)
	} else {
		rows, err = q.Query(ctx, `
			(SELECT `+paymentColumns+` FROM payments
			 WHERE sender_account_id = $1 AND (created_at, id) < ($2, $3)
			 ORDER BY created_at DESC, id DESC LIMIT $4)
			UNION ALL
			(SELECT `+paymentColumns+` FROM payments
			 WHERE receiver_account_id = $1 AND (created_at, id) < ($2, $3)
			 ORDER BY created_at DESC, id DESC LIMIT $4)
			ORDER BY created_at DESC, id DESC
			LIMIT $4`,
			accountID, after.CreatedAt, after.ID, fetch)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("payment history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan payment: %w", err)
		}
		page = append(page, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("payment history: %w", err)
	}

	if len(page) > limit {
		page = page[:limit]
		last := page[limit-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, next, nil
}
