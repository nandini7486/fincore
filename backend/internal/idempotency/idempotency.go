// Package idempotency makes money-moving requests safe to retry (NFR-01,
// Blueprint §6.3).
//
// A client sends the same Idempotency-Key with every retry of one logical
// request. The first request to claim the key does the work; any later
// request with the same key and the same content gets the stored response
// instead of moving money again.
//
// The claim is an INSERT made inside the payment transaction. If two
// requests with the same key arrive together, PostgreSQL makes the second
// INSERT wait on the primary key until the first transaction ends:
//   - first commits    -> the second sees the stored response and replays it
//   - first rolls back -> the second's INSERT succeeds and it does the work
//
// So exactly one request can move money, and a crash never leaves a stale
// "in progress" record behind (it simply rolls back).
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxKeyLength matches the CHECK constraint on idempotency_records.key.
const MaxKeyLength = 255

// ValidKey reports whether k is an acceptable Idempotency-Key: 1-255
// printable ASCII characters (UUIDs are recommended).
func ValidKey(k string) bool {
	if len(k) < 1 || len(k) > MaxKeyLength {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// Hash returns the request fingerprint: SHA-256 over the operation name and
// the canonical request fields, in a fixed order. Callers pass parsed values
// (amount in minor units, lowercase IDs), so "5.0" and "5.00" hash the same.
// Including the operation means one key reused across endpoints is a
// conflict, not a replay of the other endpoint's response.
func Hash(operation string, fields ...string) []byte {
	h := sha256.New()
	h.Write([]byte(operation))
	for _, f := range fields {
		// Length-prefix each field so ("ab","c") and ("a","bc") differ.
		fmt.Fprintf(h, "\x00%d:%s", len(f), f)
	}
	return h.Sum(nil)
}

// CanonicalID normalises a UUID for hashing.
func CanonicalID(id string) string { return strings.ToLower(id) }

// Outcome of a claim attempt.
type Outcome int

const (
	// Claimed: this request owns the key and must do the work, then Save.
	Claimed Outcome = iota
	// Replay: the same request already completed; return Stored as is.
	Replay
	// Conflict: the key was used for a different request (409).
	Conflict
)

// Stored is a completed request's response.
type Stored struct {
	Status int
	Body   []byte
}

// Claim must be the first statement of the payment transaction. On Replay
// or Conflict the caller should roll back: nothing was written.
func Claim(ctx context.Context, tx pgx.Tx, userID, key string, hash []byte) (Outcome, Stored, error) {
	// Insert the claim, or take over a record whose 24-hour window has
	// expired (the key may then be reused for a new request). A live record
	// is left alone and 0 rows are affected.
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_records (user_id, key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, key) DO UPDATE
		    SET request_hash    = EXCLUDED.request_hash,
		        payment_id      = NULL,
		        response_status = NULL,
		        response_body   = NULL,
		        created_at      = now(),
		        expires_at      = now() + interval '24 hours'
		    WHERE idempotency_records.expires_at <= now()`,
		userID, key, hash)
	if err != nil {
		return 0, Stored{}, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Claimed, Stored{}, nil
	}

	// A live record exists and its transaction has committed (the INSERT
	// waited for it). In READ COMMITTED this statement sees it.
	var storedHash, body []byte
	var status *int
	err = tx.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body
		FROM idempotency_records WHERE user_id = $1 AND key = $2`,
		userID, key).Scan(&storedHash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		// The record expired and was cleaned up between our two statements.
		// Extremely unlikely; the client can simply retry.
		return 0, Stored{}, fmt.Errorf("idempotency record vanished; retry")
	}
	if err != nil {
		return 0, Stored{}, fmt.Errorf("read idempotency record: %w", err)
	}
	if string(storedHash) != string(hash) {
		return Conflict, Stored{}, nil
	}
	if status == nil {
		// Committed records always carry a response (it is saved in the same
		// transaction as the claim), so this indicates a bug.
		return 0, Stored{}, fmt.Errorf("idempotency record has no stored response")
	}
	return Replay, Stored{Status: *status, Body: body}, nil
}

// Save records the response for a claimed key, in the same transaction as
// the money movement, so a committed claim always has a response.
func Save(ctx context.Context, tx pgx.Tx, userID, key, paymentID string, status int, body []byte) error {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_records
		SET payment_id = $3, response_status = $4, response_body = $5
		WHERE user_id = $1 AND key = $2`,
		userID, key, paymentID, status, body)
	if err != nil {
		return fmt.Errorf("save idempotency response: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("save idempotency response: key was not claimed")
	}
	return nil
}

// CleanupBatch is how many expired records one DELETE removes. Small
// batches keep each statement short, so cleanup never holds locks long
// enough to slow down payments.
const CleanupBatch = 1000

// DeleteExpired removes idempotency records whose 24-hour window has passed,
// in batches, and returns how many it deleted. Run by the worker.
func DeleteExpired(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}) (int64, error) {
	var total int64
	for {
		tag, err := q.Exec(ctx, `
			DELETE FROM idempotency_records
			WHERE (user_id, key) IN (
				SELECT user_id, key FROM idempotency_records
				WHERE expires_at <= now()
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)`, CleanupBatch)
		if err != nil {
			return total, fmt.Errorf("delete expired idempotency records: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < CleanupBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
