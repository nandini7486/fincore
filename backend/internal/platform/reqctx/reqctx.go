// Package reqctx carries per-request values (request ID, logger) in a context.
package reqctx

import (
	"context"
	"log/slog"
)

type requestIDKey struct{}

// WithRequestID returns a context carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID, or "" if none is set.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Logger returns the default logger annotated with the request ID.
func Logger(ctx context.Context) *slog.Logger {
	if id := RequestID(ctx); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}
