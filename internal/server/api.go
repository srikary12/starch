package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// APIVersion is the wire contract version. It is part of every route path so
// that a shell built against an older daemon fails loudly instead of subtly.
// See api/README.md for the contract itself.
const APIVersion = "v1"

// ErrorCode is a stable, machine-readable error identifier. Shells switch on
// these; the human-readable message is for display and may change freely.
type ErrorCode string

const (
	// ErrUnauthorized: the handshake token was missing or wrong.
	ErrUnauthorized ErrorCode = "unauthorized"
	// ErrNotFound: no such route.
	ErrNotFound ErrorCode = "not_found"
	// ErrMethodNotAllowed: the route exists but not for this method.
	ErrMethodNotAllowed ErrorCode = "method_not_allowed"
	// ErrNotImplemented: the route is part of the contract but this daemon
	// build does not serve it yet.
	ErrNotImplemented ErrorCode = "not_implemented"
)

// ErrorBody is the envelope for every non-2xx response.
type ErrorBody struct {
	Error APIError `json:"error"`
}

// APIError describes a single failure.
type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The daemon is reachable only over a 0600 socket, but a browser-driven
	// request would still be a bug worth blocking outright.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; all that is left is to record it.
		slog.Debug("writing response body", "error", err)
	}
}

// writeError writes the standard error envelope.
func writeError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	writeJSON(w, status, ErrorBody{Error: APIError{Code: code, Message: message}})
}
