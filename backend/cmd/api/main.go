// Command api is the FinCore HTTP API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nandini7486/fincore/backend/internal/httpapi"
	"github.com/nandini7486/fincore/backend/internal/platform/config"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
	"github.com/nandini7486/fincore/backend/internal/platform/metrics"
)

func main() {
	// "api healthcheck [url]" is for container HEALTHCHECKs: the image is
	// distroless (no shell, no curl), so the binary does the HTTP check.
	// Without a URL it asks this API's /readyz; the worker container passes
	// its metrics URL instead.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		target := readyzURL(os.Getenv("HTTP_ADDR"))
		if len(os.Args) > 2 {
			target = os.Args[2]
		}
		if err := healthcheck(target); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("api exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewRegistry("api")
	metrics.RegisterPool(reg, pool)
	stopMetrics, err := metrics.Serve(cfg.MetricsListenAddr(":9090"), reg)
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	// Runs after srv.Shutdown: metrics stay scrapeable while draining.
	defer stopMetrics()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(httpapi.Deps{DB: pool, Metrics: metrics.NewHTTP(reg)}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.HTTPAddr, "version", metrics.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting, let in-flight requests (and their
	// transactions) finish, then the deferred pool.Close runs.
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// readyzURL is the local /readyz URL for an HTTP_ADDR such as ":8080".
func readyzURL(addr string) string {
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr + "/readyz"
}

// healthcheck fails unless GET url returns 200.
func healthcheck(url string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return nil
}
