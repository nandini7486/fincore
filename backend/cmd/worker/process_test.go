package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/accounts"
	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/payments"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fincore-worker-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "worker")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build worker: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The real worker binary delivers a payment's notification, deletes expired
// idempotency keys, and exits cleanly on SIGTERM.
func TestWorkerBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	tdb := testdb.New(t)
	ctx := context.Background()
	count := func(sql string, args ...any) int {
		var n int
		if err := tdb.Owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Setup: two users, accounts, a top-up and a transfer (two events).
	user := func(name, email string, role auth.Role) string {
		var id auth.Identity
		if err := db.WithTx(ctx, tdb.Owner, func(tx pgx.Tx) error {
			var err error
			id, _, err = users.Create(ctx, tx, users.NewUser{Name: name, Email: email, Role: role})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id.UserID
	}
	admin := user("Admin", "admin@example.com", auth.RoleAdmin)
	alice := user("Alice", "alice@example.com", auth.RoleUser)
	bob := user("Bob", "bob@example.com", auth.RoleUser)
	acct := func(owner string) string {
		var a accounts.Account
		if err := db.WithTx(ctx, tdb.App, func(tx pgx.Tx) error {
			var err error
			a, err = accounts.Create(ctx, tx, owner, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	aliceAcct, bobAcct := acct(alice), acct(bob)
	pay := func(fn func(context.Context, pgx.Tx, payments.Request) (payments.Payment, error), r payments.Request) {
		if err := db.WithTx(ctx, tdb.App, func(tx pgx.Tx) error { _, err := fn(ctx, tx, r); return err }); err != nil {
			t.Fatal(err)
		}
	}
	pay(payments.CreateTopUp, payments.Request{ReceiverID: aliceAcct, Amount: 100000, Currency: "INR", InitiatedBy: admin})

	// An expired idempotency key for the cleanup job to find.
	if _, err := tdb.Owner.Exec(ctx, `
		INSERT INTO idempotency_records (user_id, key, request_hash, expires_at)
		VALUES ($1, 'stale-key', decode(repeat('00', 32), 'hex'), now() - interval '1 hour')`, alice); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+tdb.DSN,
		"WORKER_POLL_INTERVAL=100ms", "IDEMPOTENCY_CLEANUP_INTERVAL=200ms")
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() { exitErr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })

	// The pending top-up event is delivered once the worker starts.
	eventually(t, "top-up notification", 5*time.Second, func() bool {
		return count(`SELECT count(*) FROM notifications WHERE user_id = $1`, alice) == 1
	})
	// A payment made while the worker runs is picked up by polling.
	pay(payments.CreateTransfer, payments.Request{SenderID: aliceAcct, ReceiverID: bobAcct, Amount: 2500, Currency: "INR", InitiatedBy: alice})
	eventually(t, "transfer notification", 5*time.Second, func() bool {
		return count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND message = 'You received ₹25.00 from Alice.'`, bob) == 1
	})
	eventually(t, "expired key cleanup", 5*time.Second, func() bool {
		return count(`SELECT count(*) FROM idempotency_records WHERE key = 'stale-key'`) == 0
	})
	if n := count(`SELECT count(*) FROM outbox_events WHERE processed_at IS NULL`); n != 0 {
		t.Fatalf("pending events = %d, want 0", n)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker did not stop on SIGTERM; logs:\n%s", logs.String())
	}
	var exit *exec.ExitError
	if errors.As(exitErr, &exit) {
		t.Fatalf("worker exited with code %d; logs:\n%s", exit.ExitCode(), logs.String())
	}
	for _, want := range []string{`"msg":"worker started"`, `"msg":"deleted expired idempotency records"`, `"msg":"worker stopped"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("worker log missing %s; logs:\n%s", want, logs.String())
		}
	}
}
