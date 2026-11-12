// Package audit writes records to the append-only audit_log table
// (Blueprint §8.3).
//
// Money-moving actions call Write with the same transaction as the money
// change, so the audit record and the change commit or roll back together.
// Security events outside a transaction (auth failures, denials) use
// WriteBestEffort, which never fails the request.
package audit

import (
	"context"
	"fmt"

	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/reqctx"
)

type Outcome string

const (
	Success Outcome = "SUCCESS"
	Failure Outcome = "FAILURE"
	Denied  Outcome = "DENIED"
)

// Entry is one audit record. Details must never contain secrets or tokens.
type Entry struct {
	ActorUserID  *string // nil for anonymous or system actions
	Action       string  // e.g. "payment.create", "auth.failure"
	ResourceType string
	ResourceID   string
	Outcome      Outcome
	RequestID    string // defaults to the request ID in ctx
	IPAddress    string
	Details      map[string]any
}

// Write inserts the entry using q, which may be a transaction.
func Write(ctx context.Context, q db.DBTX, e Entry) error {
	if e.Action == "" || e.Outcome == "" {
		return fmt.Errorf("audit: action and outcome are required")
	}
	if e.RequestID == "" {
		e.RequestID = reqctx.RequestID(ctx)
	}
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_log
		    (actor_user_id, action, resource_type, resource_id, outcome, request_id, ip_address, details)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, '')::inet, $8)`,
		e.ActorUserID, e.Action, e.ResourceType, e.ResourceID, string(e.Outcome),
		e.RequestID, e.IPAddress, e.Details)
	if err != nil {
		return fmt.Errorf("write audit record %q: %w", e.Action, err)
	}
	return nil
}

// WriteBestEffort writes the entry and logs (rather than returns) any error.
func WriteBestEffort(ctx context.Context, q db.DBTX, e Entry) {
	if err := Write(ctx, q, e); err != nil {
		reqctx.Logger(ctx).Error("audit write failed", "action", e.Action, "err", err)
	}
}
