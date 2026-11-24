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
}

// Load reads configuration from the environment. DATABASE_URL is required.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      10,
		ShutdownTimeout: 10 * time.Second,
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
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
