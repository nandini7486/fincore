package notifications

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/platform/cursor"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

// Notification is one in-app message for a user.
type Notification struct {
	ID        string
	Message   string
	PaymentID string // the payment the message is about
	CreatedAt time.Time
}

// List returns up to limit of userID's notifications, newest first,
// starting after the given cursor (nil for the first page). next is nil on
// the last page. Served by notifications_user_keyset_idx (ADR 0002).
func List(ctx context.Context, q db.DBTX, userID string, after *cursor.Cursor, limit int) (page []Notification, next *cursor.Cursor, err error) {
	if limit < 1 {
		return nil, nil, fmt.Errorf("limit must be positive")
	}
	fetch := limit + 1 // one extra row tells us whether another page exists

	const cols = `n.id, n.message, e.aggregate_id, n.created_at`
	var rows pgx.Rows
	if after == nil {
		rows, err = q.Query(ctx, `
			SELECT `+cols+`
			FROM notifications n JOIN outbox_events e ON e.id = n.event_id
			WHERE n.user_id = $1
			ORDER BY n.created_at DESC, n.id DESC
			LIMIT $2`, userID, fetch)
	} else {
		rows, err = q.Query(ctx, `
			SELECT `+cols+`
			FROM notifications n JOIN outbox_events e ON e.id = n.event_id
			WHERE n.user_id = $1 AND (n.created_at, n.id) < ($2, $3)
			ORDER BY n.created_at DESC, n.id DESC
			LIMIT $4`, userID, after.CreatedAt, after.ID, fetch)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Message, &n.PaymentID, &n.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan notification: %w", err)
		}
		page = append(page, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list notifications: %w", err)
	}
	if len(page) > limit {
		page = page[:limit]
		last := page[limit-1]
		next = &cursor.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, next, nil
}
