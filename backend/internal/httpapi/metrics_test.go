package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/httpapi"
	"github.com/nandini7486/fincore/backend/internal/platform/metrics"
)

// The metrics middleware labels requests by route pattern through the real
// router, and never by raw path.
func TestRequestMetrics(t *testing.T) {
	e := setup(t)
	_, token := e.createUser(t, "metrics@example.com", auth.RoleUser)
	reg := prometheus.NewRegistry()
	h := httpapi.NewRouter(httpapi.Deps{DB: e.db.App, Metrics: metrics.NewHTTP(reg)})

	send := func(method, path, token string) int {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	send(http.MethodGet, "/healthz", "")
	send(http.MethodGet, "/healthz", "")
	send(http.MethodGet, "/api/v1/me", token)
	send(http.MethodGet, "/api/v1/me", "")                                               // 401 before routing
	send(http.MethodGet, "/api/v1/payments/00000000-0000-4000-8000-000000000001", token) // 404 from a real route
	send(http.MethodGet, "/api/v1/payments/00000000-0000-4000-8000-000000000002", token) // same series
	send(http.MethodGet, "/no/such/path/1", "")                                          // unmatched
	send(http.MethodGet, "/api/v1/no/such/path/2", token)                                // unknown path under /api/v1
	send(http.MethodDelete, "/healthz", "")                                              // 405
	send("BREW", "/healthz", "")                                                         // unknown method -> "OTHER"
	// A token-guessing attack shows up as 401s on /api/v1/*, not hidden in
	// "unmatched", and random paths cannot add series.

	want := `
# HELP fincore_http_requests_total HTTP requests by method, route pattern and status code.
# TYPE fincore_http_requests_total counter
fincore_http_requests_total{method="DELETE",route="unmatched",status="405"} 1
fincore_http_requests_total{method="GET",route="/api/v1/*",status="401"} 1
fincore_http_requests_total{method="GET",route="/api/v1/*",status="404"} 1
fincore_http_requests_total{method="GET",route="/api/v1/me",status="200"} 1
fincore_http_requests_total{method="GET",route="/api/v1/payments/{paymentID}",status="404"} 2
fincore_http_requests_total{method="GET",route="/healthz",status="200"} 2
fincore_http_requests_total{method="GET",route="unmatched",status="404"} 1
fincore_http_requests_total{method="OTHER",route="unmatched",status="405"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "fincore_http_requests_total"); err != nil {
		t.Fatal(err)
	}
	// Two payment IDs, random paths: still one series per method and route.
	if n := testutil.CollectAndCount(reg, "fincore_http_request_duration_seconds"); n != 7 {
		t.Fatalf("duration series = %d, want 7 (one per method+route)", n)
	}
	if v := gaugeValue(t, reg, "fincore_http_requests_in_flight"); v != 0 {
		t.Fatalf("in flight after all requests = %v, want 0", v)
	}
}

// /metrics is internal: the public API router must not serve it.
func TestMetricsNotOnPublicRouter(t *testing.T) {
	h := httpapi.NewRouter(httpapi.Deps{Metrics: metrics.NewHTTP(prometheus.NewRegistry())})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on the API router = %d, want 404", rec.Code)
	}
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
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
	t.Fatalf("metric %s not registered", name)
	return 0
}
