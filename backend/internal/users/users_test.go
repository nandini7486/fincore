package users_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

func create(t *testing.T, tdb *testdb.DB, in users.NewUser) (auth.Identity, string, error) {
	t.Helper()
	var id auth.Identity
	var token string
	err := db.WithTx(context.Background(), tdb.Owner, func(tx pgx.Tx) error {
		var err error
		id, token, err = users.Create(context.Background(), tx, in)
		return err
	})
	return id, token, err
}

func TestCreate(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	id, token, err := create(t, tdb, users.NewUser{Name: " Alice ", Email: "Alice@Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if id.Name != "Alice" || id.Role != auth.RoleUser || id.UserID == "" {
		t.Fatalf("unexpected identity %+v", id)
	}

	// The token works, and only its hash is stored.
	got, err := auth.LookupByToken(ctx, tdb.App, token)
	if err != nil || got != id {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	var stored []byte
	if err := tdb.Owner.QueryRow(ctx, `SELECT api_token_hash FROM users WHERE id = $1`, id.UserID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == token {
		t.Fatal("raw token stored")
	}

	// Creation is audited.
	var n int
	if err := tdb.Owner.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'user.create' AND resource_id = $1`, id.UserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit records = %d, want 1", n)
	}

	// Email uniqueness is case-insensitive.
	if _, _, err := create(t, tdb, users.NewUser{Name: "Other", Email: "alice@example.COM"}); !errors.Is(err, users.ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v, want ErrEmailTaken", err)
	}
}

func TestCreateValidation(t *testing.T) {
	tdb := testdb.New(t)
	for _, in := range []users.NewUser{
		{Name: "", Email: "a@b.c"},
		{Name: "A", Email: "not-an-email"},
		{Name: "A", Email: "a@b.c", Role: "ROOT"},
	} {
		if _, _, err := create(t, tdb, in); !errors.Is(err, users.ErrInvalidInput) {
			t.Errorf("Create(%+v) err = %v, want ErrInvalidInput", in, err)
		}
	}
}
