package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
	"github.com/nandini7486/fincore/backend/internal/platform/reqctx"
)

const requestIDHeader = "X-Request-ID"

// requestID accepts a well-formed client X-Request-ID or generates one,
// stores it in the context, and echoes it in the response.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
	})
}

// validRequestID allows 1-128 chars of [A-Za-z0-9._-] so client-supplied
// IDs cannot inject anything into logs.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// accessLog writes one structured log line per request. It never logs
// headers or bodies, so tokens cannot leak into logs.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		reqctx.Logger(r.Context()).LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.Status()),
			slog.Int("bytes", ww.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// recoverer turns a panic into a logged 500 instead of a dropped connection.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				reqctx.Logger(r.Context()).Error("panic", "panic", rec, "stack", string(debug.Stack()))
				httpx.WriteError(w, r, http.StatusInternalServerError, "INTERNAL", "An unexpected error occurred.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
