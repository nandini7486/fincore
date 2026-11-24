// Command worker delivers outbox events and runs housekeeping jobs.
//
// It polls outbox_events (Blueprint §8.5), turning payment.succeeded and
// topup.succeeded events into in-app notifications, and periodically deletes
// expired idempotency records. Several workers can run at once: events are
// claimed with FOR UPDATE SKIP LOCKED.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/worker
//
// Optional: WORKER_POLL_INTERVAL (default 1s), IDEMPOTENCY_CLEANUP_INTERVAL
// (default 1m), DB_MAX_CONNS.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nandini7486/fincore/backend/internal/idempotency"
	"github.com/nandini7486/fincore/backend/internal/notifications"
	"github.com/nandini7486/fincore/backend/internal/outbox"
	"github.com/nandini7486/fincore/backend/internal/platform/config"
	"github.com/nandini7486/fincore/backend/internal/platform/db"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("worker exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// SIGTERM/SIGINT cancel ctx: the worker finishes the event it is
	// handling (its transaction commits or rolls back), then exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	w := &outbox.Worker{DB: pool, Handlers: notifications.Handlers()}
	slog.Info("worker started", "poll_interval", cfg.WorkerPollInterval, "cleanup_interval", cfg.CleanupInterval)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.Run(ctx, cfg.WorkerPollInterval)
	}()
	go func() {
		defer wg.Done()
		cleanupLoop(ctx, pool, cfg.CleanupInterval)
	}()
	wg.Wait()
	slog.Info("worker stopped")
	return nil
}

func cleanupLoop(ctx context.Context, pool *pgxpool.Pool, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		n, err := idempotency.DeleteExpired(ctx, pool)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("idempotency cleanup failed", "err", err, "deleted", n)
		case n > 0:
			slog.Info("deleted expired idempotency records", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
