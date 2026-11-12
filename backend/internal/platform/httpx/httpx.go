// Package httpx contains small HTTP helpers shared by all handlers:
// JSON responses, the standard error envelope, and client IP extraction.
package httpx

import (
	"encoding/json"
	"net"
	"net/http"

	"github.com/nandini7486/fincore/backend/internal/platform/reqctx"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrorBody is the standard error envelope (Blueprint §7).
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes the standard error envelope. Messages must be safe to
// show to clients: never include internal error text.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: reqctx.RequestID(r.Context()),
	}})
}

// WriteInternalError logs err with the request ID and returns a generic 500.
func WriteInternalError(w http.ResponseWriter, r *http.Request, err error) {
	reqctx.Logger(r.Context()).Error("internal error", "err", err, "path", r.URL.Path)
	WriteError(w, r, http.StatusInternalServerError, "INTERNAL", "An unexpected error occurred.")
}

// ClientIP returns the TCP peer address. Proxy headers such as
// X-Forwarded-For are deliberately ignored until a trusted proxy exists.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}
