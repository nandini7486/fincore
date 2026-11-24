// Package notifications turns outbox events into in-app notifications
// (Blueprint §8.6). In V1 the receiver of money gets a message such as
// "You received ₹2,000.00 from Alice."
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/money"
	"github.com/nandini7486/fincore/backend/internal/outbox"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/ids"
)

// Handlers returns the outbox handlers that create notifications.
func Handlers() map[string]outbox.Handler {
	return map[string]outbox.Handler{
		outbox.PaymentSucceeded: notifyReceiver,
		outbox.TopUpSucceeded:   notifyReceiver,
	}
}

// notifyReceiver writes one notification for the owner of the receiving
// account. It is idempotent: notifications.event_id is UNIQUE, so a
// redelivered event inserts nothing (ON CONFLICT DO NOTHING).
func notifyReceiver(ctx context.Context, tx pgx.Tx, e outbox.Event) error {
	var ev payments.SucceededEvent
	if err := json.Unmarshal(e.Payload, &ev); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	// Validate before querying, so a bad event is recorded with a clear
	// reason instead of a raw SQL error.
	if (ev.Type != payments.Transfer && ev.Type != payments.TopUp) ||
		!ids.IsUUID(ev.SenderAccountID) || !ids.IsUUID(ev.ReceiverAccountID) || ev.AmountMinor <= 0 {
		return fmt.Errorf("%s payload is incomplete or invalid", e.Type)
	}

	// The receiver's owner and, for transfers, the sender's display name.
	var receiverUser *string
	var senderName *string
	err := tx.QueryRow(ctx, `
		SELECT r.user_id, su.name
		FROM accounts r
		LEFT JOIN accounts s ON s.id = $2
		LEFT JOIN users su ON su.id = s.user_id
		WHERE r.id = $1`,
		ev.ReceiverAccountID, ev.SenderAccountID,
	).Scan(&receiverUser, &senderName)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("receiver account %s not found", ev.ReceiverAccountID)
	}
	if err != nil {
		return fmt.Errorf("look up receiver: %w", err)
	}
	if receiverUser == nil {
		return fmt.Errorf("receiver account %s has no owner", ev.ReceiverAccountID)
	}

	msg := Message(ev, senderName)
	_, err = tx.Exec(ctx, `
		INSERT INTO notifications (user_id, event_id, message)
		VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING`,
		*receiverUser, e.ID, msg)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

// Message is the text shown to the receiver.
func Message(ev payments.SucceededEvent, senderName *string) string {
	amount := money.FormatINR(ev.AmountMinor)
	if ev.Type == payments.TopUp {
		return amount + " was added to your account."
	}
	if senderName != nil && *senderName != "" {
		return fmt.Sprintf("You received %s from %s.", amount, *senderName)
	}
	return fmt.Sprintf("You received %s.", amount)
}
