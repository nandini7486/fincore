package config

import "testing"

func TestMetricsListenAddr(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", ":9090"}, // unset: the binary's default
		{"off", ""},   // disabled
		{"127.0.0.1:9999", "127.0.0.1:9999"},
	} {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("METRICS_ADDR", tc.env)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.MetricsListenAddr(":9090"); got != tc.want {
			t.Errorf("METRICS_ADDR=%q: got %q, want %q", tc.env, got, tc.want)
		}
	}
}
