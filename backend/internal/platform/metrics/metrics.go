// Package metrics exposes Prometheus metrics (Blueprint NFR-07).
//
// Metrics are served on their own listener (METRICS_ADDR, default :9090),
// never on the public API port: they are internal operational data, so they
// need no bearer token and cannot leak through the public listener.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version is the build version, set at link time:
//
//	go build -ldflags "-X github.com/nandini7486/fincore/backend/internal/platform/metrics.Version=v1.0.0"
var Version = "dev"

// NewRegistry returns a registry with Go runtime, process and build-info
// metrics. component is "api" or "worker".
func NewRegistry(component string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "fincore_build_info",
			Help:        "Always 1; labels identify the running build.",
			ConstLabels: prometheus.Labels{"version": Version, "component": component},
		}, func() float64 { return 1 }),
	)
	return reg
}

// UnmatchedRoute is the route label for requests that matched no route.
// Using the raw path instead would let anyone create unlimited time series
// by requesting random URLs.
const UnmatchedRoute = "unmatched"

// HTTP records request counts, latency and in-flight requests.
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewHTTP registers the HTTP metrics on reg.
func NewHTTP(reg prometheus.Registerer) *HTTP {
	h := &HTTP{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fincore_http_requests_total",
			Help: "HTTP requests by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fincore_http_request_duration_seconds",
			Help: "HTTP request latency by method and route pattern.",
			// 1ms .. 10s: payments are expected in single-digit ms, lock
			// waits up to the 2s lock_timeout, and anything above is a bug.
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fincore_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
	reg.MustRegister(h.requests, h.duration, h.inFlight)
	return h
}

// Middleware must run inside the chi router (r.Use) so the matched route
// pattern is known once the handler returns.
func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.inFlight.Inc()
		defer h.inFlight.Dec()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := routeLabel(r)
		method := r.Method
		if !knownMethod(method) {
			method = "OTHER"
		}
		h.requests.WithLabelValues(method, route, strconv.Itoa(ww.Status())).Inc()
		h.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	})
}

// routeLabel is the chi route pattern that served the request, such as
// /api/v1/payments/{paymentID}. Patterns are fixed strings from the router,
// so the number of series stays bounded whatever paths clients send:
//
//   - a 404 from a real route (a payment that does not exist) keeps it;
//   - /api/v1/* means the request stopped inside the API sub-router before
//     a specific route matched: a 401 from the auth middleware, or an
//     unknown path under /api/v1;
//   - "unmatched" means no route at all (unknown path, or 405).
func routeLabel(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return UnmatchedRoute
	}
	p := rc.RoutePattern()
	if p == "" {
		return UnmatchedRoute
	}
	return p
}

func knownMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

// RegisterPool exposes pgxpool statistics: how many connections are in use,
// and how often and how long requests waited for one. Rising wait time is
// the first sign the pool (DB_MAX_CONNS) is too small for the load.
func RegisterPool(reg prometheus.Registerer, pool *pgxpool.Pool) {
	stat := func(f func(*pgxpool.Stat) float64) func() float64 {
		return func() float64 { return f(pool.Stat()) }
	}
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "fincore_db_pool_max_conns", Help: "Maximum pool size."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "fincore_db_pool_total_conns", Help: "Open connections."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.TotalConns()) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "fincore_db_pool_acquired_conns", Help: "Connections currently in use."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "fincore_db_pool_idle_conns", Help: "Idle connections."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "fincore_db_pool_acquires_total", Help: "Successful connection acquires."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquireCount()) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "fincore_db_pool_empty_acquires_total", Help: "Acquires that had to wait because no connection was free."},
			stat(func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "fincore_db_pool_acquire_wait_seconds_total", Help: "Total time spent waiting for a connection."},
			stat(func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() })),
	)
}

// Handler serves the registry in Prometheus text format.
func Handler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	return mux
}

// Serve runs the metrics listener until ctx is cancelled. An empty addr
// disables it. The listener is bound before Serve returns, so a bad address
// fails at startup rather than silently.
func Serve(addr string, reg *prometheus.Registry) (stop func(), err error) {
	if addr == "" {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: Handler(reg), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		slog.Info("metrics listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server failed", "err", err)
		}
	}()
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-done
	}, nil
}
