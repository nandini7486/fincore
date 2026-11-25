package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Outbox records what the worker's handlers did. Results are counted when
// the handler returns, before the transaction commits, so they count handler
// outcomes, not committed deliveries; the backlog gauges below are the
// source of truth for what is still pending.
type Outbox struct {
	handled  *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewOutbox registers worker metrics on reg, including backlog gauges that
// query outbox_events at scrape time.
func NewOutbox(reg prometheus.Registerer, pool *pgxpool.Pool) *Outbox {
	o := &Outbox{
		handled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fincore_outbox_events_handled_total",
			Help: "Outbox events handed to a handler, by event type and result (ok or error).",
		}, []string{"event_type", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fincore_outbox_handler_duration_seconds",
			Help:    "Time spent in the event handler.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"event_type"}),
	}
	reg.MustRegister(o.handled, o.duration, &backlog{pool: pool})
	return o
}

// Observe matches outbox.Worker.Observe.
func (o *Outbox) Observe(eventType string, err error, d time.Duration) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	o.handled.WithLabelValues(eventType, result).Inc()
	o.duration.WithLabelValues(eventType).Observe(d.Seconds())
}

var (
	pendingDesc = prometheus.NewDesc("fincore_outbox_pending_events",
		"Events not yet processed (including those waiting for a retry).", nil, nil)
	failingDesc = prometheus.NewDesc("fincore_outbox_failing_events",
		"Pending events whose last attempt failed.", nil, nil)
	oldestDesc = prometheus.NewDesc("fincore_outbox_oldest_pending_age_seconds",
		"Age of the oldest pending event; 0 when none. The main lag alert signal.", nil, nil)
	scrapeErrDesc = prometheus.NewDesc("fincore_outbox_backlog_scrape_error",
		"1 if the backlog query failed during this scrape.", nil, nil)
)

// backlog reads the outbox backlog from the database on every scrape, so it
// is correct across several workers and after restarts.
type backlog struct{ pool *pgxpool.Pool }

func (b *backlog) Describe(ch chan<- *prometheus.Desc) {
	ch <- pendingDesc
	ch <- failingDesc
	ch <- oldestDesc
	ch <- scrapeErrDesc
}

func (b *backlog) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var pending, failing int64
	var oldest float64
	err := b.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE last_error IS NOT NULL),
		       coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
		FROM outbox_events WHERE processed_at IS NULL`).Scan(&pending, &failing, &oldest)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(scrapeErrDesc, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(scrapeErrDesc, prometheus.GaugeValue, 0)
	ch <- prometheus.MustNewConstMetric(pendingDesc, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(failingDesc, prometheus.GaugeValue, float64(failing))
	ch <- prometheus.MustNewConstMetric(oldestDesc, prometheus.GaugeValue, oldest)
}
