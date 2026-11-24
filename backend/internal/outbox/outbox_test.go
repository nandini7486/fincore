package outbox_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nandini7486/fincore/backend/internal/outbox"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
)

// These tests use their own handlers and a test table, so they exercise the
// worker mechanics independently of notifications.

func setup(t *testing.T) *testdb.DB {
	t.Helper()
	tdb := testdb.New(t)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE test_deliveries (event_id uuid NOT NULL, worker int NOT NULL)`,
		`GRANT SELECT, INSERT ON test_deliveries TO fincore_app`,
	} {
		if _, err := tdb.Owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	return tdb
}

func write(t *testing.T, tdb *testdb.DB, eventType string, n int) []string {
	t.Helper()
	var ids []string
	err := db.WithTx(context.Background(), tdb.App, func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			if err := outbox.Write(context.Background(), tx, eventType, "00000000-0000-4000-8000-000000000001", map[string]int{"n": i}); err != nil {
				return err
			}
		}
		// Only this call's events (each test uses distinct event types per call).
		rows, err := tx.Query(context.Background(), `SELECT id FROM outbox_events WHERE event_type = $1 ORDER BY created_at, id`, eventType)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// record is a handler that logs each delivery in test_deliveries.
func record(worker int) outbox.Handler {
	return func(ctx context.Context, tx pgx.Tx, e outbox.Event) error {
		_, err := tx.Exec(ctx, `INSERT INTO test_deliveries (event_id, worker) VALUES ($1, $2)`, e.ID, worker)
		return err
	}
}

func count(t *testing.T, tdb *testdb.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tdb.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type eventState struct {
	processed bool
	attempts  int
	lastError *string
	dueIn     time.Duration
}

func state(t *testing.T, tdb *testdb.DB, id string) eventState {
	t.Helper()
	var s eventState
	var dueIn float64
	err := tdb.Owner.QueryRow(context.Background(), `
		SELECT processed_at IS NOT NULL, attempts, last_error,
		       extract(epoch FROM next_attempt_at - now())
		FROM outbox_events WHERE id = $1`, id).Scan(&s.processed, &s.attempts, &s.lastError, &dueIn)
	if err != nil {
		t.Fatal(err)
	}
	s.dueIn = time.Duration(dueIn * float64(time.Second))
	return s
}

func TestWriteIsPartOfTheCallersTransaction(t *testing.T) {
	tdb := setup(t)
	boom := errors.New("payment failed later")
	err := db.WithTx(context.Background(), tdb.App, func(tx pgx.Tx) error {
		if err := outbox.Write(context.Background(), tx, "x.happened", "00000000-0000-4000-8000-000000000001", map[string]string{}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := count(t, tdb, `SELECT count(*) FROM outbox_events`); n != 0 {
		t.Fatalf("events after rollback = %d, want 0", n)
	}
}

func TestDeliversEachEventAndMarksItProcessed(t *testing.T) {
	tdb := setup(t)
	ids := write(t, tdb, "x.happened", 3)
	w := &outbox.Worker{DB: tdb.App, Handlers: map[string]outbox.Handler{"x.happened": record(1)}}

	n, err := w.Drain(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("Drain = %d, %v; want 3", n, err)
	}
	for _, id := range ids {
		s := state(t, tdb, id)
		if !s.processed || s.attempts != 1 || s.lastError != nil {
			t.Fatalf("event %s state = %+v", id, s)
		}
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries`); n != 3 {
		t.Fatalf("deliveries = %d, want 3", n)
	}
	// Nothing left to do.
	if n, _ := w.Drain(context.Background()); n != 0 {
		t.Fatalf("second Drain processed %d, want 0", n)
	}
}

// A failing handler's writes are undone, the failure is recorded, and the
// event is retried after the backoff, then succeeds.
func TestFailedEventIsRetriedWithBackoff(t *testing.T) {
	tdb := setup(t)
	id := write(t, tdb, "x.happened", 1)[0]
	var fail atomic.Bool
	fail.Store(true)
	w := &outbox.Worker{
		DB: tdb.App,
		Handlers: map[string]outbox.Handler{"x.happened": func(ctx context.Context, tx pgx.Tx, e outbox.Event) error {
			if err := record(1)(ctx, tx, e); err != nil {
				return err
			}
			if fail.Load() {
				return errors.New("downstream unavailable")
			}
			return nil
		}},
		Backoff: func(attempts int) time.Duration { return time.Duration(attempts) * time.Hour },
	}

	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := state(t, tdb, id)
	if s.processed || s.attempts != 1 || s.lastError == nil || *s.lastError != "downstream unavailable" {
		t.Fatalf("after failure: %+v", s)
	}
	if s.dueIn < 59*time.Minute || s.dueIn > 61*time.Minute {
		t.Fatalf("retry due in %v, want ~1h (backoff)", s.dueIn)
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries`); n != 0 {
		t.Fatalf("the failed handler's write survived (%d rows); it must be rolled back", n)
	}
	// Not due yet: a drain does nothing.
	if n, _ := w.Drain(context.Background()); n != 0 {
		t.Fatalf("event retried before its backoff (%d)", n)
	}

	// Time passes and the downstream recovers.
	if _, err := tdb.Owner.Exec(context.Background(), `UPDATE outbox_events SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	if n, err := w.Drain(context.Background()); err != nil || n != 1 {
		t.Fatalf("retry Drain = %d, %v", n, err)
	}
	s = state(t, tdb, id)
	if !s.processed || s.attempts != 2 || s.lastError != nil {
		t.Fatalf("after retry: %+v", s)
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries`); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
}

func TestDefaultBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second,
		8: 128 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 50: 5 * time.Minute, 1000: 5 * time.Minute,
	} {
		if got := outbox.DefaultBackoff(attempts); got != want {
			t.Errorf("DefaultBackoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

// A poison event (a panicking handler, or a type nobody handles) is recorded
// and retried later, and never blocks the events behind it.
func TestPoisonEventsDoNotBlockOthers(t *testing.T) {
	tdb := setup(t)
	panicky := write(t, tdb, "x.panics", 1)[0]
	unknown := write(t, tdb, "x.unknown", 1)
	good := write(t, tdb, "x.happened", 2)
	_ = unknown
	w := &outbox.Worker{
		DB: tdb.App,
		Handlers: map[string]outbox.Handler{
			"x.happened": record(1),
			"x.panics":   func(context.Context, pgx.Tx, outbox.Event) error { panic("bad payload") },
		},
		Backoff: func(int) time.Duration { return time.Hour },
	}
	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if s := state(t, tdb, panicky); s.processed || s.lastError == nil || *s.lastError != "handler panicked: bad payload" {
		t.Fatalf("panicking event: %+v", s)
	}
	var unknownErr string
	if err := tdb.Owner.QueryRow(context.Background(),
		`SELECT last_error FROM outbox_events WHERE event_type = 'x.unknown'`).Scan(&unknownErr); err != nil {
		t.Fatal(err)
	}
	if unknownErr != `no handler registered for event type: "x.unknown"` {
		t.Fatalf("unknown event last_error = %q", unknownErr)
	}
	for _, id := range good {
		if !state(t, tdb, id).processed {
			t.Fatal("a good event behind a poison event was not processed")
		}
	}
}

// Several workers draining at once must each take a DIFFERENT event at the
// same time (FOR UPDATE SKIP LOCKED), and every event must be delivered
// exactly once.
//
// The concurrency is forced, not hoped for: each handler holds its event
// until all workers are holding one simultaneously. That can only happen if
// the workers skip each other's locked rows. With plain FOR UPDATE they
// would queue behind the first worker's row instead, the barrier would never
// fill, and the test fails after 3 seconds.
func TestCompetingWorkersDeliverEachEventOnce(t *testing.T) {
	tdb := setup(t)
	const events, workers = 300, 6
	write(t, tdb, "x.happened", events)

	var (
		inFlight   atomic.Int32 // handlers currently holding an event
		allHeld    atomic.Bool  // all workers held an event at the same moment
		release    = make(chan struct{})
		releaseOne sync.Once
	)
	handler := func(worker int) outbox.Handler {
		return func(ctx context.Context, tx pgx.Tx, e outbox.Event) error {
			if err := record(worker)(ctx, tx, e); err != nil {
				return err
			}
			if inFlight.Add(1) == workers {
				allHeld.Store(true)
				releaseOne.Do(func() { close(release) })
			}
			defer inFlight.Add(-1)
			select {
			case <-release:
			case <-time.After(3 * time.Second):
				releaseOne.Do(func() { close(release) }) // give up; the assertion below fails
			}
			return nil
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &outbox.Worker{DB: tdb.App, Handlers: map[string]outbox.Handler{"x.happened": handler(i)}}
			if _, err := w.Drain(context.Background()); err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if !allHeld.Load() {
		t.Fatalf("the %d workers never held %d different events at once: they blocked on each other's locks (is SKIP LOCKED missing?)", workers, workers)
	}
	if n := count(t, tdb, `SELECT count(DISTINCT worker) FROM test_deliveries`); n != workers {
		t.Fatalf("%d workers delivered events, want all %d", n, workers)
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries`); n != events {
		t.Fatalf("deliveries = %d, want %d", n, events)
	}
	if n := count(t, tdb, `SELECT count(DISTINCT event_id) FROM test_deliveries`); n != events {
		t.Fatalf("distinct events delivered = %d, want %d (an event was delivered twice)", n, events)
	}
	if n := count(t, tdb, `SELECT count(*) FROM outbox_events WHERE processed_at IS NULL`); n != 0 {
		t.Fatalf("pending events = %d, want 0", n)
	}
}

// The worker dies while handling an event (connection killed mid-handler).
// Its transaction rolls back: the handler's write and the "processed" mark
// both disappear, no failure is recorded, and the next worker delivers the
// event exactly once.
func TestWorkerCrashMidHandlerRedeliversOnce(t *testing.T) {
	tdb := setup(t)
	id := write(t, tdb, "x.happened", 1)[0]

	started := make(chan int, 1)
	crashing := &outbox.Worker{DB: tdb.App, Handlers: map[string]outbox.Handler{
		"x.happened": func(ctx context.Context, tx pgx.Tx, e outbox.Event) error {
			if err := record(1)(ctx, tx, e); err != nil {
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			started <- pid
			_, err := tx.Exec(ctx, `SELECT pg_sleep(10)`) // "working"; killed here
			return err
		},
	}}
	done := make(chan error, 1)
	go func() { _, err := crashing.ProcessOne(context.Background()); done <- err }()

	pid := <-started
	if _, err := tdb.Owner.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("ProcessOne succeeded although its connection was killed")
	}

	s := state(t, tdb, id)
	if s.processed || s.attempts != 0 || s.lastError != nil {
		t.Fatalf("after crash: %+v; the crashed attempt must leave no trace", s)
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries`); n != 0 {
		t.Fatalf("crashed handler's write survived (%d)", n)
	}

	healthy := &outbox.Worker{DB: tdb.App, Handlers: map[string]outbox.Handler{"x.happened": record(2)}}
	if n, err := healthy.Drain(context.Background()); err != nil || n != 1 {
		t.Fatalf("redelivery Drain = %d, %v", n, err)
	}
	if n := count(t, tdb, `SELECT count(*) FROM test_deliveries WHERE event_id = $1`, id); n != 1 {
		t.Fatalf("deliveries after redelivery = %d, want 1", n)
	}
	if !state(t, tdb, id).processed {
		t.Fatal("event not processed after redelivery")
	}
}

// Run keeps polling: an event written while the worker is running is
// delivered without anyone calling Drain, and cancellation stops it.
func TestRunPollsUntilCancelled(t *testing.T) {
	tdb := setup(t)
	w := &outbox.Worker{DB: tdb.App, Handlers: map[string]outbox.Handler{"x.happened": record(1)}}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx, 20*time.Millisecond); close(stopped) }()

	write(t, tdb, "x.happened", 1)
	deadline := time.Now().Add(3 * time.Second)
	for count(t, tdb, `SELECT count(*) FROM test_deliveries`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("running worker never delivered the new event")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}
