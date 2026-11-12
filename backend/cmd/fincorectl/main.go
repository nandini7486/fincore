// Command fincorectl is FinCore's operator CLI.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/fincorectl user create -name "Alice" -email alice@example.com [-role ADMIN]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/users"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fincorectl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 || args[0] != "user" || args[1] != "create" {
		return fmt.Errorf("usage: fincorectl user create -name NAME -email EMAIL [-role USER|ADMIN]")
	}

	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email address")
	role := fs.String("role", "USER", "USER or ADMIN")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	var id auth.Identity
	var token string
	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		id, token, err = users.Create(ctx, tx, users.NewUser{Name: *name, Email: *email, Role: auth.Role(*role)})
		return err
	})
	if err != nil {
		return err
	}

	fmt.Printf("Created user %s (%s, %s)\n", id.UserID, id.Email, id.Role)
	fmt.Printf("API token: %s\n", token)
	fmt.Println("Store this token now. It is not saved and cannot be shown again.")
	return nil
}
