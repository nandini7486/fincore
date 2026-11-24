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
	// PaymentID is set on 422 responses: the business-rule failure was
	// stored as a FAILED payment the client can look up.
	PaymentID string `json:"payment_id,omitempty"`
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

// StatusClientClosedRequest is the non-standard 499 (nginx convention): the
// client disconnected before the server finished. Nobody receives it; it
// exists so logs and metrics can tell client disconnects from server faults.
const StatusClientClosedRequest = 499

// WriteInternalError logs err with the request ID and returns a generic 500.
//
// If the client has already gone away (its request context is cancelled),
// the failure was caused by the disconnect, not by a server fault: the work
// was rolled back, so this is logged as a warning with status 499 instead.
func WriteInternalError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		reqctx.Logger(r.Context()).Warn("client disconnected before completion; work rolled back",
			"err", err, "path", r.URL.Path)
		WriteError(w, r, StatusClientClosedRequest, "CLIENT_CLOSED_REQUEST", "The client closed the request.")
		return
	}
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
