// Package config loads process configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	DBMaxConns      int32
	ShutdownTimeout time.Duration

	// metricsAddr is METRICS_ADDR as given; see MetricsListenAddr.
	metricsAddr string

	// Worker settings (cmd/worker).
	WorkerPollInterval time.Duration // how often to look for outbox events
	CleanupInterval    time.Duration // how often to delete expired idempotency keys
}

// Load reads configuration from the environment. DATABASE_URL is required.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      10,
		ShutdownTimeout: 10 * time.Second,
		metricsAddr:     os.Getenv("METRICS_ADDR"),

		WorkerPollInterval: time.Second,
		CleanupInterval:    time.Minute,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		cfg.DBMaxConns = int32(n)
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"WORKER_POLL_INTERVAL", &cfg.WorkerPollInterval},
		{"IDEMPOTENCY_CLEANUP_INTERVAL", &cfg.CleanupInterval},
	} {
		if v := os.Getenv(d.key); v != "" {
			dur, err := time.ParseDuration(v)
			if err != nil || dur <= 0 {
				return Config{}, fmt.Errorf("%s must be a positive duration such as 1s or 500ms, got %q", d.key, v)
			}
			*d.dst = dur
		}
	}
	return cfg, nil
}

// MetricsListenAddr is where the internal /metrics listener binds: def when
// METRICS_ADDR is unset, "" (disabled) when it is "off".
func (c Config) MetricsListenAddr(def string) string {
	switch c.metricsAddr {
	case "":
		return def
	case "off":
		return ""
	}
	return c.metricsAddr
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
