package main

// Process-level failure tests (Blueprint §10, NFR-06). These build and run
// the real API binary, then kill it or shut it down in the middle of a
// payment, and check what reached the database.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/reconcile"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
	"github.com/nandini7486/fincore/backend/internal/users"
)

// appName tags the server's database connections so tests can find them.
const appName = "fincore-api-under-test"

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fincore-api-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "api")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build api: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer collects the server's log output safely across goroutines.
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

type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	base string
	logs *syncBuffer
	// exited is closed when the process ends; exitErr is then its result.
	// A closed channel can be waited on any number of times.
	exited  chan struct{}
	exitErr error
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start runs the real binary against dsn and waits until it is ready.
func start(t *testing.T, dsn string) *server {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", appName)
	u.RawQuery = q.Encode()

	addr := freePort(t)
	s := &server{t: t, base: "http://" + addr, logs: &syncBuffer{}, exited: make(chan struct{})}
	s.cmd = exec.Command(binary)
	s.cmd.Env = append(os.Environ(), "DATABASE_URL="+u.String(), "HTTP_ADDR="+addr, "METRICS_ADDR=127.0.0.1:0")
	s.cmd.Stdout, s.cmd.Stderr = s.logs, s.logs
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.exitErr = s.cmd.Wait(); close(s.exited) }()
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		<-s.exited
		if strings.Contains(s.logs.String(), "fct_") {
			t.Error("a bearer token appeared in the server log")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Get(s.base + "/readyz"); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not become ready; logs:\n%s", s.logs.String())
	return nil
}

type response struct {
	status   int
	replayed bool
	body     map[string]any
}

func (s *server) do(method, path, token, key, body string) (response, error) {
	req, err := http.NewRequest(method, s.base+path, strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := response{status: res.StatusCode, replayed: res.Header.Get("Idempotent-Replayed") == "true"}
	_ = json.Unmarshal(raw, &r.body)
	return r, nil
}

func (s *server) must(method, path, token, key, body string, want int) map[string]any {
	s.t.Helper()
	r, err := s.do(method, path, token, key, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if r.status != want {
		s.t.Fatalf("%s %s = %d %v, want %d", method, path, r.status, r.body, want)
	}
	return r.body
}

// world is a funded test setup: alice has ₹100.00, bob has an account.
type world struct {
	db                        *testdb.DB
	adminToken, aliceToken    string
	bobToken                  string
	aliceAcct, bobAcct, payTo string
}

func newWorld(t *testing.T, s *server, tdb *testdb.DB) *world {
	t.Helper()
	w := &world{db: tdb}
	token := func(email string, role auth.Role) string {
		var tok string
		err := db.WithTx(context.Background(), tdb.Owner, func(tx pgx.Tx) error {
			var err error
			_, tok, err = users.Create(context.Background(), tx, users.NewUser{Name: "T", Email: email, Role: role})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	w.adminToken = token("admin@example.com", auth.RoleAdmin)
	w.aliceToken = token("alice@example.com", auth.RoleUser)
	w.bobToken = token("bob@example.com", auth.RoleUser)
	w.aliceAcct = s.must("POST", "/api/v1/accounts", w.aliceToken, "", "", 201)["account_id"].(string)
	w.bobAcct = s.must("POST", "/api/v1/accounts", w.bobToken, "", "", 201)["account_id"].(string)
	s.must("POST", "/api/v1/accounts/"+w.aliceAcct+"/topups", w.adminToken, "topup-1", `{"amount":"100.00","currency":"INR"}`, 201)
	w.payTo = fmt.Sprintf(`{"sender_account_id":%q,"receiver_account_id":%q,"amount":"10.00","currency":"INR"}`, w.aliceAcct, w.bobAcct)
	return w
}

func (w *world) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *world) aliceBalance(t *testing.T) int64 {
	t.Helper()
	var b int64
	if err := w.db.Owner.QueryRow(context.Background(), `SELECT balance FROM accounts WHERE id = $1`, w.aliceAcct).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (w *world) assertBooksBalance(t *testing.T) {
	t.Helper()
	rep, err := reconcile.Run(context.Background(), w.db.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("books do not balance: %+v", rep)
	}
}

// holdAliceLock locks alice's account row from another session, so a
// payment stops mid-transaction (key claimed, nothing else written yet).
func (w *world) holdAliceLock(t *testing.T) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, w.aliceAcct); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitForPaymentBlocked waits until the server's payment transaction is
// waiting on the account lock (well inside the 2s lock_timeout).
func (w *world) waitForPaymentBlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if w.count(t, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'`, appName) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("payment never blocked on the account lock")
}

// The server process is killed (OOM killer, crash, kill -9) in the middle of
// a payment transaction. It never sent COMMIT, so PostgreSQL rolls the
// transaction back when the connection dies. After a restart, the client's
// retry with the same Idempotency-Key is processed normally and charges once.
func TestServerKilledMidPaymentThenRestarted(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	tdb := testdb.New(t)
	s := start(t, tdb.DSN)
	w := newWorld(t, s, tdb)
	key := "kill-test-key"

	release := w.holdAliceLock(t)
	clientErr := make(chan error, 1)
	go func() {
		_, err := s.do("POST", "/api/v1/payments", w.aliceToken, key, w.payTo)
		clientErr <- err
	}()
	w.waitForPaymentBlocked(t)

	if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-s.exited
	if err := <-clientErr; err == nil {
		t.Fatal("client got a response from a killed server")
	}
	release()

	// Every connection of the dead process disappears; the in-flight
	// transaction goes with it.
	deadline := time.Now().Add(5 * time.Second)
	for w.count(t, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, appName) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("connections of the killed server are still open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := w.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER'`); n != 0 {
		t.Fatalf("transfers after kill = %d, want 0", n)
	}
	if n := w.count(t, `SELECT count(*) FROM idempotency_records WHERE key = $1`, key); n != 0 {
		t.Fatal("the killed request's idempotency claim survived")
	}
	if n := w.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'payment.succeeded'`); n != 0 {
		t.Fatal("an outbox event survived the killed payment")
	}
	if got := w.aliceBalance(t); got != 10000 {
		t.Fatalf("alice = %d after kill, want 10000", got)
	}
	w.assertBooksBalance(t)

	// Restart and retry exactly as the client would.
	s2 := start(t, tdb.DSN)
	r, err := s2.do("POST", "/api/v1/payments", w.aliceToken, key, w.payTo)
	if err != nil || r.status != http.StatusCreated || r.replayed {
		t.Fatalf("retry after restart = %d replayed=%v err=%v, want a fresh 201", r.status, r.replayed, err)
	}
	if got := w.aliceBalance(t); got != 9000 {
		t.Fatalf("alice = %d after retry, want 9000 (charged once)", got)
	}
	w.assertBooksBalance(t)
}

// On SIGTERM (a deploy or scale-down) the server stops accepting new
// connections but lets the in-flight payment finish and commit, then exits
// cleanly (NFR-06).
func TestGracefulShutdownFinishesInFlightPayment(t *testing.T) {
	if testing.Short() {
		t.Skip("process test")
	}
	tdb := testdb.New(t)
	s := start(t, tdb.DSN)
	w := newWorld(t, s, tdb)

	release := w.holdAliceLock(t)
	type result struct {
		r   response
		err error
	}
	inFlight := make(chan result, 1)
	go func() {
		r, err := s.do("POST", "/api/v1/payments", w.aliceToken, "graceful-key", w.payTo)
		inFlight <- result{r, err}
	}()
	w.waitForPaymentBlocked(t)

	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// New connections are refused as soon as shutdown begins.
	refused := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		res, err := (&http.Client{Timeout: 200 * time.Millisecond}).Get(s.base + "/healthz")
		if err != nil {
			refused = true
			break
		}
		res.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if !refused {
		t.Fatal("server still accepted new connections after SIGTERM")
	}

	release() // let the in-flight payment take its lock and finish
	got := <-inFlight
	if got.err != nil || got.r.status != http.StatusCreated {
		t.Fatalf("in-flight payment = %d %v err=%v, want 201", got.r.status, got.r.body, got.err)
	}

	select {
	case <-s.exited:
		var exit *exec.ExitError
		if errors.As(s.exitErr, &exit) {
			t.Fatalf("server exited with code %d, want 0; logs:\n%s", exit.ExitCode(), s.logs.String())
		}
		if s.exitErr != nil {
			t.Fatal(s.exitErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit after finishing the in-flight request")
	}

	if !strings.Contains(s.logs.String(), `"msg":"shutting down"`) {
		t.Errorf("no shutdown log line; logs:\n%s", s.logs.String())
	}
	if got := w.aliceBalance(t); got != 9000 {
		t.Fatalf("alice = %d, want 9000: the in-flight payment must have committed", got)
	}
	if n := w.count(t, `SELECT count(*) FROM payments WHERE type = 'TRANSFER' AND status = 'SUCCESS'`); n != 1 {
		t.Fatalf("successful transfers = %d, want 1", n)
	}
	w.assertBooksBalance(t)
}
