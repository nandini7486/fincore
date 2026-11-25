package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/nandini7486/fincore/backend/internal/outbox"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/metrics"
	"github.com/nandini7486/fincore/backend/internal/testutil/testdb"
)

// Worker metrics: handler results are counted, and the backlog gauges read
// the real outbox table, so a failing event shows as pending and failing.
func TestOutboxMetrics(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	om := metrics.NewOutbox(reg, tdb.App)

	write := func(typ string) {
		if err := db.WithTx(ctx, tdb.App, func(tx pgx.Tx) error {
			return outbox.Write(ctx, tx, typ, "00000000-0000-4000-8000-000000000001", map[string]int{"n": 1})
		}); err != nil {
			t.Fatal(err)
		}
	}
	backlog := `
# HELP fincore_outbox_failing_events Pending events whose last attempt failed.
# TYPE fincore_outbox_failing_events gauge
fincore_outbox_failing_events %d
# HELP fincore_outbox_pending_events Events not yet processed (including those waiting for a retry).
# TYPE fincore_outbox_pending_events gauge
fincore_outbox_pending_events %d
# HELP fincore_outbox_backlog_scrape_error 1 if the backlog query failed during this scrape.
# TYPE fincore_outbox_backlog_scrape_error gauge
fincore_outbox_backlog_scrape_error 0
`
	check := func(pending, failing int) {
		t.Helper()
		exp := fmt.Sprintf(backlog, failing, pending)
		if err := testutil.GatherAndCompare(reg, strings.NewReader(exp),
			"fincore_outbox_pending_events", "fincore_outbox_failing_events", "fincore_outbox_backlog_scrape_error"); err != nil {
			t.Fatal(err)
		}
	}

	check(0, 0)
	write("good")
	write("good")
	write("bad")
	check(3, 0)

	w := &outbox.Worker{
		DB: tdb.App,
		Handlers: map[string]outbox.Handler{
			"good": func(context.Context, pgx.Tx, outbox.Event) error { return nil },
			"bad":  func(context.Context, pgx.Tx, outbox.Event) error { return errors.New("boom") },
		},
		Observe: om.Observe,
	}
	if _, err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	check(1, 1) // the bad event waits for a retry

	want := `
# HELP fincore_outbox_events_handled_total Outbox events handed to a handler, by event type and result (ok or error).
# TYPE fincore_outbox_events_handled_total counter
fincore_outbox_events_handled_total{event_type="bad",result="error"} 1
fincore_outbox_events_handled_total{event_type="good",result="ok"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "fincore_outbox_events_handled_total"); err != nil {
		t.Fatal(err)
	}

	// The oldest pending event's age is reported; it is the lag signal.
	if _, err := tdb.Owner.Exec(ctx, `UPDATE outbox_events SET created_at = now() - interval '90 seconds' WHERE processed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if age := value(t, reg, "fincore_outbox_oldest_pending_age_seconds"); age < 89 || age > 120 {
		t.Fatalf("oldest pending age = %v, want about 90", age)
	}

	// If the database is unreachable the scrape still succeeds and says so.
	tdb.App.Close()
	if v := value(t, reg, "fincore_outbox_backlog_scrape_error"); v != 1 {
		t.Fatalf("scrape error with a closed pool = %v, want 1", v)
	}
}

func TestPoolMetricsAndHandler(t *testing.T) {
	tdb := testdb.New(t)
	reg := metrics.NewRegistry("api")
	metrics.RegisterPool(reg, tdb.App)
	if err := tdb.App.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(metrics.Handler(reg))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d", res.StatusCode)
	}
	for _, want := range []string{
		`fincore_build_info{component="api",version="dev"} 1`,
		fmt.Sprintf("fincore_db_pool_max_conns %d", tdb.App.Stat().MaxConns()),
		"fincore_db_pool_acquires_total ",
		"fincore_db_pool_acquire_wait_seconds_total ",
		"go_goroutines ",
		"process_resident_memory_bytes ",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// Only /metrics is served on the internal listener.
	if res, err := http.Get(srv.URL + "/api/v1/me"); err != nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("other path on metrics listener: %v %v", res.StatusCode, err)
	}
}

func TestServe(t *testing.T) {
	stop, err := metrics.Serve("", prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("empty addr should disable, got %v", err)
	}
	stop()
	if _, err := metrics.Serve("127.0.0.1:not-a-port", prometheus.NewRegistry()); err == nil {
		t.Fatal("a bad address must fail at startup")
	}
}

func value(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}
