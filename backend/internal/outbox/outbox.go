// Package outbox implements the transactional outbox (Blueprint §8.5).
//
// The problem it solves: after a payment commits, other things must happen
// (notify the receiver; later webhooks, emails, the AI service). Doing them
// directly is a "dual write": if the payment commits and the process dies
// before the notification is sent, it is lost; if the notification is sent
// and the payment rolls back, it is a lie.
//
// Instead, the payment transaction also inserts an event row into
// outbox_events. The event exists if and only if the payment committed. A
// separate worker later reads pending events and hands each to a handler.
//
// Delivery is at-least-once: a handler may see the same event again (for
// example after a crash between handling and marking it processed, for
// handlers with external side effects). Handlers must therefore be
// idempotent; the notifications handler uses UNIQUE (event_id).
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

// Event types written in V1.
const (
	PaymentSucceeded = "payment.succeeded"
	TopUpSucceeded   = "topup.succeeded"
)

// Event is one row of outbox_events as seen by a handler.
type Event struct {
	ID          string
	Type        string
	AggregateID string // the payment ID
	Payload     json.RawMessage
	Attempts    int // previous attempts, 0 on first delivery
	CreatedAt   time.Time
}

// Write inserts an event. It must be called with the transaction that makes
// the change the event describes, so both commit or neither does.
func Write(ctx context.Context, tx pgx.Tx, eventType, aggregateID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (event_type, aggregate_id, payload) VALUES ($1, $2, $3)`,
		eventType, aggregateID, body); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// Handler processes one event inside the worker's transaction. Database
// writes made through tx commit together with marking the event processed.
// It must be idempotent: the same event may be delivered more than once.
type Handler func(ctx context.Context, tx pgx.Tx, e Event) error

// Pool is what the worker needs from *pgxpool.Pool.
type Pool interface {
	db.TxBeginner
}

// Worker delivers pending outbox events to handlers.
type Worker struct {
	DB       Pool
	Handlers map[string]Handler
	// Backoff returns the delay before retrying an event that has failed
	// attempts times. Defaults to DefaultBackoff.
	Backoff func(attempts int) time.Duration
	Logger  *slog.Logger
	// Observe, if set, is called once per claimed event after its handler
	// ran, with the handler's error (nil on success) and duration. It is
	// how cmd/worker records metrics without this package knowing about
	// Prometheus.
	Observe func(eventType string, err error, d time.Duration)
}

// DefaultBackoff doubles from 1s per failed attempt (1s, 2s, 4s ... 256s),
// capped at 5 minutes from the 10th attempt on.
func DefaultBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 20 { // avoid shifting into overflow; long past the cap anyway
		return 5 * time.Minute
	}
	d := time.Second << (attempts - 1)
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}

// ErrNoHandler is recorded as last_error for an unknown event type.
var ErrNoHandler = errors.New("no handler registered for event type")

// ProcessOne claims and handles a single due event, in its own transaction.
// It returns false when no event is due.
//
// FOR UPDATE SKIP LOCKED lets several workers run at once: each claims a
// different event, and an event being handled by one worker is invisible
// to the others. If the worker dies mid-way, the transaction rolls back,
// the row lock is released and the event is simply picked up again.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	found := false
	err := db.WithTx(ctx, w.DB, func(tx pgx.Tx) error {
		var e Event
		err := tx.QueryRow(ctx, `
			SELECT id, event_type, aggregate_id, payload, attempts, created_at
			FROM outbox_events
			WHERE processed_at IS NULL AND next_attempt_at <= now()
			ORDER BY next_attempt_at, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED`,
		).Scan(&e.ID, &e.Type, &e.AggregateID, &e.Payload, &e.Attempts, &e.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim outbox event: %w", err)
		}
		found = true

		// The handler runs inside a savepoint so a failure undoes only its
		// own writes, and the failure itself can still be recorded.
		if _, err := tx.Exec(ctx, "SAVEPOINT handler"); err != nil {
			return err
		}
		start := time.Now()
		herr := w.handle(ctx, tx, e)
		if w.Observe != nil {
			w.Observe(e.Type, herr, time.Since(start))
		}
		if herr == nil {
			_, err = tx.Exec(ctx, `
				UPDATE outbox_events
				SET processed_at = now(), attempts = attempts + 1, last_error = NULL
				WHERE id = $1`, e.ID)
			return err
		}

		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT handler"); err != nil {
			return err
		}
		attempts := e.Attempts + 1
		backoff := w.Backoff
		if backoff == nil {
			backoff = DefaultBackoff
		}
		delay := backoff(attempts)
		w.logger().Warn("outbox event failed; will retry",
			"event_id", e.ID, "event_type", e.Type, "attempts", attempts, "retry_in", delay, "err", herr)
		_, err = tx.Exec(ctx, `
			UPDATE outbox_events
			SET attempts = $2, last_error = $3, next_attempt_at = now() + $4::interval
			WHERE id = $1`,
			e.ID, attempts, truncate(herr.Error(), 1000), fmt.Sprintf("%d milliseconds", delay.Milliseconds()))
		return err
	})
	return found, err
}

// handle runs the event's handler, turning a panic into an error so one bad
// event cannot crash the worker.
func (w *Worker) handle(ctx context.Context, tx pgx.Tx, e Event) (err error) {
	h, ok := w.Handlers[e.Type]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoHandler, e.Type)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return h(ctx, tx, e)
}

// Drain processes due events until none are left (or ctx ends) and returns
// how many were claimed.
func (w *Worker) Drain(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		found, err := w.ProcessOne(ctx)
		if err != nil {
			return n, err
		}
		if !found {
			return n, nil
		}
		n++
	}
	return n, ctx.Err()
}

// Run drains due events every pollInterval until ctx is cancelled. Errors
// (such as the database being briefly unreachable) are logged and retried
// on the next tick rather than stopping the worker.
func (w *Worker) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if n, err := w.Drain(ctx); err != nil && ctx.Err() == nil {
			w.logger().Error("outbox drain failed", "err", err, "processed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
