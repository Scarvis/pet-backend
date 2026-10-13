// Package common holds helpers shared by every domain package: typed errors,
// money/amount conversions and the Action HTTP middleware.
package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrRetryable is a sentinel wrapped around transient failures such as an
// exhausted SERIALIZABLE retry loop (ARCHITECTURE section 8.0). The HTTP layer
// maps any error that wraps it to 409 Conflict so the client knows to retry.
var ErrRetryable = errors.New("retryable")

// IsRetryable reports whether err, or anything it wraps, is retryable.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// Wrap marks err as retryable while preserving the chain for errors.Is/As.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRetryable, err)
}

// Error is a structured, serializable error returned by handlers so API clients
// get a consistent JSON body instead of a stack trace (section 10). The router
// renders it with Status and Code; Message is the human-readable detail.
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// New builds an Error with an explicit status, code and message.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// BadRequest maps to HTTP 400.
func BadRequest(message string) *Error { return New(http.StatusBadRequest, "bad_request", message) }

// NotFound maps to HTTP 404.
func NotFound(message string) *Error { return New(http.StatusNotFound, "not_found", message) }

// Conflict maps to HTTP 409 and is also marked retryable (section 8.0): a handler
// that exhausts its SERIALIZABLE retries returns this so the client can back off.
func Conflict(message string) *Error {
	return New(http.StatusConflict, "conflict", message)
}

// Unwrap lets errors.Is find ErrRetryable when a conflict error is wrapped.
func (e *Error) Unwrap() error { return ErrRetryable }

// HTTPStatus maps an error to the HTTP status it should be rendered with: a typed
// *Error wins, then a retryable error becomes 409, otherwise 500 (section 10).
func HTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	if IsRetryable(err) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// writeJSONError renders err (or a generic message) as a JSON error body and sets
// the HTTP status. Used by the router and the Action middleware (section 10).
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// WriteError renders err as a JSON error body using HTTPStatus for the status
// code. A typed *Error carries its own code/message; any other error becomes
// a 500 internal_error (or 409 when it wraps ErrRetryable).
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if errors.As(err, &e) {
		writeJSONError(w, e.Status, e.Code, e.Message)
		return
	}
	if IsRetryable(err) {
		writeJSONError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// WriteJSON writes v as a JSON response body with status 200.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
