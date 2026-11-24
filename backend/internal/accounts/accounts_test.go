package accounts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

func newUser(t *testing.T, tdb *testdb.DB, email string) string {
	t.Helper()
	var id auth.Identity
	err := db.WithTx(context.Background(), tdb.Owner, func(tx pgx.Tx) error {
		var err error
		id, _, err = users.Create(context.Background(), tx, users.NewUser{Name: "Test", Email: email})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id.UserID
}

// create runs accounts.Create as fincore_app, the API's real privileges.
func create(t *testing.T, tdb *testdb.DB, ownerID, currency string) (accounts.Account, error) {
	t.Helper()
	var a accounts.Account
	err := db.WithTx(context.Background(), tdb.App, func(tx pgx.Tx) error {
		var err error
		a, err = accounts.Create(context.Background(), tx, ownerID, currency)
		return err
	})
	return a, err
}

func count(t *testing.T, tdb *testdb.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tdb.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreate(t *testing.T) {
	tdb := testdb.New(t)
	alice := newUser(t, tdb, "alice@example.com")

	a, err := create(t, tdb, alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.UserID == nil || *a.UserID != alice {
		t.Fatalf("unexpected owner: %+v", a)
	}
	if a.Type != "USER" || a.Currency != "INR" || a.Balance != 0 || a.Status != "ACTIVE" {
		t.Fatalf("unexpected account: %+v", a)
	}

	// Creation is audited with the actor and the new account's ID.
	n := count(t, tdb, `
		SELECT count(*) FROM audit_log
		WHERE action = 'account.create' AND outcome = 'SUCCESS'
		  AND actor_user_id = $1 AND resource_id = $2`, alice, a.ID)
	if n != 1 {
		t.Fatalf("audit records = %d, want 1", n)
	}

	// An explicit INR currency is accepted too.
	if _, err := create(t, tdb, alice, "INR"); err != nil {
		t.Fatalf("explicit INR: %v", err)
	}
}

func TestCreateRejectsUnsupportedCurrency(t *testing.T) {
	tdb := testdb.New(t)
	alice := newUser(t, tdb, "alice@example.com")

	for _, c := range []string{"USD", "inr", "INRR"} {
		if _, err := create(t, tdb, alice, c); !errors.Is(err, accounts.ErrInvalidInput) {
			t.Errorf("currency %q: err = %v, want ErrInvalidInput", c, err)
		}
	}
	if n := count(t, tdb, `SELECT count(*) FROM accounts WHERE user_id = $1`, alice); n != 0 {
		t.Fatalf("accounts created = %d, want 0", n)
	}
}

// If the surrounding transaction fails, neither the account nor its audit
// record may survive: they commit together or not at all.
func TestCreateRollsBackWithAudit(t *testing.T) {
	tdb := testdb.New(t)
	alice := newUser(t, tdb, "alice@example.com")
	boom := errors.New("later step failed")

	err := db.WithTx(context.Background(), tdb.App, func(tx pgx.Tx) error {
		if _, err := accounts.Create(context.Background(), tx, alice, ""); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := count(t, tdb, `SELECT count(*) FROM accounts WHERE user_id = $1`, alice); n != 0 {
		t.Fatalf("accounts = %d after rollback, want 0", n)
	}
	if n := count(t, tdb, `SELECT count(*) FROM audit_log WHERE action = 'account.create'`); n != 0 {
		t.Fatalf("audit records = %d after rollback, want 0", n)
	}
}

func TestGetForOwner(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()
	alice := newUser(t, tdb, "alice@example.com")
	bob := newUser(t, tdb, "bob@example.com")

	a, err := create(t, tdb, alice, "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := accounts.GetForOwner(ctx, tdb.App, a.ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.Balance != 0 {
		t.Fatalf("got %+v, want %+v", got, a)
	}

	if _, err := accounts.GetForOwner(ctx, tdb.App, a.ID, bob); !errors.Is(err, accounts.ErrNotOwner) {
		t.Fatalf("other user: err = %v, want ErrNotOwner", err)
	}

	var treasury string
	if err := tdb.Owner.QueryRow(ctx, `SELECT id FROM accounts WHERE type = 'TREASURY'`).Scan(&treasury); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.GetForOwner(ctx, tdb.App, treasury, alice); !errors.Is(err, accounts.ErrNotOwner) {
		t.Fatalf("treasury: err = %v, want ErrNotOwner", err)
	}

	missing := "00000000-0000-4000-8000-000000000000"
	if _, err := accounts.GetForOwner(ctx, tdb.App, missing, alice); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}
}
