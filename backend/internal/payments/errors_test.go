package payments_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nandini7486/fincore/backend/internal/payments"
)

// Lock and statement timeouts are safe to retry (nothing committed), so
// they become ErrBusy (503). Everything else passes through unchanged.
func TestMapDBError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		busy bool
	}{
		{"lock_timeout", &pgconn.PgError{Code: "55P03"}, true},
		{"statement_timeout", &pgconn.PgError{Code: "57014"}, true},
		{"wrapped lock_timeout", fmt.Errorf("lock accounts: %w", &pgconn.PgError{Code: "55P03"}), true},
		{"deadlock (must not happen: locks are ordered)", &pgconn.PgError{Code: "40P01"}, false},
		{"check violation", &pgconn.PgError{Code: "23514"}, false},
		{"connection terminated", &pgconn.PgError{Code: "57P01"}, false},
		{"plain error", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := payments.MapDBError(tc.err)
			if errors.Is(got, payments.ErrBusy) != tc.busy {
				t.Fatalf("MapDBError(%v) busy = %v, want %v", tc.err, !tc.busy, tc.busy)
			}
			if !tc.busy && got != tc.err {
				t.Fatalf("non-busy error changed: %v", got)
			}
		})
	}
}
