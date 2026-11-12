// Command migrate applies FinCore's database migrations.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/migrate [up|down|reset|status]
//
// "down" rolls back one migration; "reset" rolls back all of them.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/nandini7486/fincore/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	p, err := migrations.NewProvider(db)
	if err != nil {
		return err
	}

	ctx := context.Background()
	switch cmd {
	case "up":
		results, err := p.Up(ctx)
		for _, r := range results {
			fmt.Println(r)
		}
		return err
	case "down":
		r, err := p.Down(ctx)
		if r != nil {
			fmt.Println(r)
		}
		return err
	case "reset":
		results, err := p.DownTo(ctx, 0)
		for _, r := range results {
			fmt.Println(r)
		}
		return err
	case "status":
		statuses, err := p.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			applied := "pending"
			if s.State == goose.StateApplied {
				applied = "applied at " + s.AppliedAt.Format("2006-01-02 15:04:05")
			}
			fmt.Printf("%05d  %-45s %s\n", s.Source.Version, s.Source.Path, applied)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q (want up, down, reset, status)", cmd)
	}
}
