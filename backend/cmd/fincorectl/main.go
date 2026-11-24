// Command fincorectl is FinCore's operator CLI.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/fincorectl user create -name "Alice" -email alice@example.com [-role ADMIN]
//	DATABASE_URL=postgres://... go run ./cmd/fincorectl reconcile [-json]
//
// reconcile exits 0 when the books balance, 1 when it finds a problem and
// 2 when it cannot run, so it can be used from cron or CI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/reconcile"
	"github.com/nandini7486/fincore/backend/internal/users"
)

const usage = `usage:
  fincorectl user create -name NAME -email EMAIL [-role USER|ADMIN]
  fincorectl reconcile [-json]`

// errBooksUnbalanced makes reconcile exit 1 rather than 2.
var errBooksUnbalanced = errors.New("reconciliation found problems")

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil:
	case errors.Is(err, errBooksUnbalanced):
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "fincorectl:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	switch {
	case len(args) >= 2 && args[0] == "user" && args[1] == "create":
		return userCreate(args[2:])
	case len(args) >= 1 && args[0] == "reconcile":
		return reconcileCmd(args[1:])
	default:
		return errors.New(usage)
	}
}

func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}
	return db.NewPool(ctx, dsn, 2)
}

func userCreate(args []string) error {

	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email address")
	role := fs.String("role", "USER", "USER or ADMIN")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := openPool(ctx)
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

// reconcileCmd checks that the books balance and prints a report.
func reconcileCmd(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := openPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	rep, err := reconcile.Run(ctx, pool)
	if err != nil {
		return err
	}

	if *asJSON {
		out := struct {
			OK bool `json:"ok"`
			reconcile.Report
		}{rep.OK(), rep}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printReport(rep)
	}
	if !rep.OK() {
		return errBooksUnbalanced
	}
	return nil
}

func printReport(r reconcile.Report) {
	fmt.Printf("Checked %d accounts, %d payments, %d ledger entries.\n",
		r.AccountsChecked, r.PaymentsChecked, r.LedgerEntries)
	fmt.Printf("Ledger sum: %d (must be 0)\n", r.LedgerSum)
	fmt.Printf("Accounts whose balance disagrees with the ledger: %d\n", r.AccountMismatchCount)
	for _, m := range r.AccountMismatches {
		fmt.Printf("  %s (%s): stored %d, ledger %d, difference %d\n",
			m.AccountID, m.Type, m.StoredBalance, m.LedgerBalance, m.StoredBalance-m.LedgerBalance)
	}
	fmt.Printf("Payments with wrong ledger entries: %d\n", r.PaymentProblemCount)
	for _, p := range r.PaymentProblems {
		fmt.Printf("  %s (%s): %s\n", p.PaymentID, p.Status, p.Problem)
	}
	if shown := int64(len(r.AccountMismatches)); shown < r.AccountMismatchCount {
		fmt.Printf("(showing the first %d account mismatches)\n", shown)
	}
	if shown := int64(len(r.PaymentProblems)); shown < r.PaymentProblemCount {
		fmt.Printf("(showing the first %d payment problems)\n", shown)
	}
	if r.OK() {
		fmt.Println("OK: the books balance.")
	} else {
		fmt.Println("PROBLEMS FOUND: investigate before moving more money.")
	}
}
