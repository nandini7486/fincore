// Package testdb gives each test package its own throwaway PostgreSQL
// database, so packages can run in parallel without interfering.
//
// Tests are skipped unless FINCORE_TEST_DATABASE_URL points at a server
// where the user may CREATE DATABASE (CI provides one).
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/nandini7486/fincore/backend/migrations"
)

const EnvVar = "FINCORE_TEST_DATABASE_URL"

// NewEmpty creates an empty database and returns its DSN. It is dropped
// when the test finishes.
func NewEmpty(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skipf("%s not set; skipping database tests", EnvVar)
	}
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	defer conn.Close(ctx)

	// fincore_app is cluster-wide. Create it up front, tolerating a
	// concurrent creator, so parallel migrations never race on it.
	_, err = conn.Exec(ctx, `
		DO $$
		BEGIN
			CREATE ROLE fincore_app NOLOGIN;
		EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
		END
		$$`)
	if err != nil {
		t.Fatalf("testdb: ensure role: %v", err)
	}

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "fincore_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			t.Logf("testdb: drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("testdb: drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("testdb: parse %s: %v", EnvVar, err)
	}
	u.Path = "/" + name
	return u.String()
}

// DB is a fully migrated test database.
type DB struct {
	DSN string
	// Owner connects as the migration owner (superuser in CI). Use it
	// for fixtures and assertions.
	Owner *pgxpool.Pool
	// App runs every connection as fincore_app, exactly the privileges
	// the API has in production. Use it for the code under test.
	App *pgxpool.Pool
}

// New creates a database with all migrations applied.
func New(t testing.TB) *DB {
	t.Helper()
	ctx := context.Background()
	dsn := NewEmpty(t)

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testdb: open: %v", err)
	}
	p, err := migrations.NewProvider(sqlDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}
	_ = sqlDB.Close()

	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: owner pool: %v", err)
	}
	t.Cleanup(owner.Close)

	appCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	appCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE fincore_app")
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatalf("testdb: app pool: %v", err)
	}
	t.Cleanup(app.Close)

	return &DB{DSN: dsn, Owner: owner, App: app}
}
