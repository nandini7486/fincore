package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("healthcheck asked %s, want /readyz", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if got := readyzURL(":8080"); got != "http://127.0.0.1:8080/readyz" {
		t.Fatalf("readyzURL(:8080) = %s", got)
	}
	url := readyzURL(addr)

	if err := healthcheck(url); err != nil {
		t.Fatalf("ready server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(url); err == nil {
		t.Fatal("503 from /readyz must fail the healthcheck")
	}
	srv.Close()
	if err := healthcheck(url); err == nil {
		t.Fatal("a stopped server must fail the healthcheck")
	}
}
