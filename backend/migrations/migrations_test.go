package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/nandini7486/fincore/backend/migrations"
)

// These tests need a disposable PostgreSQL database. They reset the schema.
// CI provides one; locally: docker compose up -d postgres, then set
// FINCORE_TEST_DATABASE_URL.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("FINCORE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FINCORE_TEST_DATABASE_URL not set; skipping database tests")
	}
	return dsn
}

const (
	sqlstateCheckViolation   = "23514"
	sqlstateUniqueViolation  = "23505"
	sqlstateInsufficientPriv = "42501"
)

func TestMigrations(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}

	// Round trip: start clean, apply all, roll all back, apply again.
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("initial reset: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	t.Run("treasury seeded once", func(t *testing.T) {
		var n int
		err := conn.QueryRow(ctx,
			`SELECT count(*) FROM accounts WHERE type = 'TREASURY' AND currency = 'INR'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("treasury accounts = %d, want 1", n)
		}
	})

	t.Run("second treasury rejected", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			_, err := tx.Exec(ctx, `INSERT INTO accounts (type, currency) VALUES ('TREASURY', 'INR')`)
			wantSQLState(t, err, sqlstateUniqueViolation)
		})
	})

	t.Run("user balance cannot go negative", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			_, err := tx.Exec(ctx, `UPDATE accounts SET balance = -1 WHERE id = $1`, f.alice)
			wantSQLState(t, err, sqlstateCheckViolation)
		})
	})

	t.Run("treasury may go negative", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			_, err := tx.Exec(ctx, `UPDATE accounts SET balance = -500000 WHERE type = 'TREASURY'`)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})

	t.Run("payment to self rejected", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			_, err := tx.Exec(ctx, `
				INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
				                      status, initiated_by_user_id, request_id)
				VALUES ('TRANSFER', $1, $1, 100, 'INR', 'SUCCESS', $2, 'req-test')`, f.alice, f.user)
			wantSQLState(t, err, sqlstateCheckViolation)
		})
	})

	t.Run("failed payment requires reason", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			_, err := tx.Exec(ctx, `
				INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
				                      status, initiated_by_user_id, request_id)
				VALUES ('TRANSFER', $1, $2, 100, 'INR', 'FAILED', $3, 'req-test')`, f.alice, f.bob, f.user)
			wantSQLState(t, err, sqlstateCheckViolation)
		})
	})

	t.Run("non-positive amount rejected", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			_, err := tx.Exec(ctx, `
				INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
				                      status, initiated_by_user_id, request_id)
				VALUES ('TRANSFER', $1, $2, 0, 'INR', 'SUCCESS', $3, 'req-test')`, f.alice, f.bob, f.user)
			wantSQLState(t, err, sqlstateCheckViolation)
		})
	})

	t.Run("only one debit per payment", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			_, err := tx.Exec(ctx, `
				INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
				VALUES ($1, $2, 'DEBIT', 100, 'INR')`, f.payment, f.alice)
			wantSQLState(t, err, sqlstateUniqueViolation)
		})
	})

	t.Run("duplicate idempotency key rejected", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			hash := make([]byte, 32)
			q := `INSERT INTO idempotency_records (user_id, key, request_hash) VALUES ($1, 'k1', $2)`
			if _, err := tx.Exec(ctx, q, f.user, hash); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(ctx, q, f.user, hash)
			wantSQLState(t, err, sqlstateUniqueViolation)
		})
	})

	// Append-only tables: even the owner is stopped by the trigger.
	for _, stmt := range []string{
		`UPDATE ledger_entries SET amount = 1`,
		`DELETE FROM ledger_entries`,
		`TRUNCATE ledger_entries`,
		`UPDATE audit_log SET outcome = 'SUCCESS'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		t.Run("trigger blocks: "+stmt, func(t *testing.T) {
			inTx(t, conn, func(tx pgx.Tx) {
				seed(t, tx)
				_, err := tx.Exec(ctx, stmt)
				wantSQLState(t, err, sqlstateInsufficientPriv)
				if !strings.Contains(err.Error(), "append-only") {
					t.Fatalf("expected trigger error, got %v", err)
				}
			})
		})
	}

	// The application role is never granted UPDATE/DELETE on append-only tables.
	for _, stmt := range []string{
		`UPDATE ledger_entries SET amount = 1`,
		`DELETE FROM ledger_entries`,
		`UPDATE audit_log SET outcome = 'SUCCESS'`,
		`DELETE FROM audit_log`,
	} {
		t.Run("fincore_app denied: "+stmt, func(t *testing.T) {
			inTx(t, conn, func(tx pgx.Tx) {
				seed(t, tx)
				if _, err := tx.Exec(ctx, `SET LOCAL ROLE fincore_app`); err != nil {
					t.Fatal(err)
				}
				_, err := tx.Exec(ctx, stmt)
				wantSQLState(t, err, sqlstateInsufficientPriv)
				if !strings.Contains(err.Error(), "permission denied") {
					t.Fatalf("expected permission error, got %v", err)
				}
			})
		})
	}

	t.Run("fincore_app can insert ledger and audit rows", func(t *testing.T) {
		inTx(t, conn, func(tx pgx.Tx) {
			f := seed(t, tx)
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE fincore_app`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO audit_log (actor_user_id, action, outcome, request_id)
				VALUES ($1, 'payment.create', 'SUCCESS', 'req-test')`, f.user); err != nil {
				t.Fatalf("audit insert: %v", err)
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil {
				t.Fatalf("ledger select: %v", err)
			}
		})
	})
}

type fixture struct {
	user, alice, bob, payment string
}

// seed inserts a user with two accounts, a successful payment between
// them with both ledger entries, and one audit record.
func seed(t *testing.T, tx pgx.Tx) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture

	err := tx.QueryRow(ctx, `
		INSERT INTO users (name, email, api_token_hash)
		VALUES ('Test User', 'test@example.com', sha256('token'::bytea))
		RETURNING id`).Scan(&f.user)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for _, dst := range []*string{&f.alice, &f.bob} {
		err := tx.QueryRow(ctx, `
			INSERT INTO accounts (user_id, type, currency, balance)
			VALUES ($1, 'USER', 'INR', 100000) RETURNING id`, f.user).Scan(dst)
		if err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO payments (type, sender_account_id, receiver_account_id, amount, currency,
		                      status, initiated_by_user_id, request_id)
		VALUES ('TRANSFER', $1, $2, 100, 'INR', 'SUCCESS', $3, 'req-test')
		RETURNING id`, f.alice, f.bob, f.user).Scan(&f.payment)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (payment_id, account_id, entry_type, amount, currency)
		VALUES ($1, $2, 'DEBIT', 100, 'INR'), ($1, $3, 'CREDIT', 100, 'INR')`,
		f.payment, f.alice, f.bob)
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_log (actor_user_id, action, outcome) VALUES ($1, 'payment.create', 'SUCCESS')`,
		f.user)
	if err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	return f
}

// inTx runs fn in a transaction that is always rolled back, so each
// subtest starts from the freshly migrated state.
func inTx(t *testing.T, conn *pgx.Conn, fn func(pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(tx)
}

func wantSQLState(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got no error", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected PostgreSQL error %s, got %v", code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, code)
	}
}
