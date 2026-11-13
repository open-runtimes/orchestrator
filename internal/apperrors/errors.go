// Package apperrors provides structured application errors with HTTP status mapping.
package apperrors

import (
	"errors"
	"fmt"
)

// Sentinel errors for classification via errors.Is().
var (
	ErrValidation  = errors.New("validation error")
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("conflict")
	ErrExhausted   = errors.New("exhausted")
	ErrUnavailable = errors.New("unavailable")
	ErrInternal    = errors.New("internal error")
)

// Error is a classified error: the sentinel says which HTTP status it maps to,
// the message is what the client sees.
type Error struct {
	Sentinel error  // Wrapped sentinel for errors.Is() classification
	Message  string // Human-readable message
	Field    string // For validation errors (e.g., "id", "image")
	Cause    error  // Underlying error, for Internal
}

// Error returns the human-readable error message.
func (e *Error) Error() string {
	return e.Message
}

// Unwrap exposes both the sentinel and the cause, so errors.Is sees the
// classification and the underlying error (context.Canceled, a K8s status)
// alike.
func (e *Error) Unwrap() []error {
	return []error{e.Sentinel, e.Cause}
}

// Validation creates a validation error for a specific field.
func Validation(field, message string) error {
	return &Error{Sentinel: ErrValidation, Message: message, Field: field}
}

// NotFound creates a not found error for a resource.
func NotFound(resource, id string) error {
	return &Error{Sentinel: ErrNotFound, Message: fmt.Sprintf("%s %s not found", resource, id)}
}

// Conflict creates a conflict error.
func Conflict(reason string) error {
	return &Error{Sentinel: ErrConflict, Message: reason}
}

// Exhausted creates a capacity-exhausted error: the request was valid but no
// capacity is free to serve it right now (e.g. a warm pool with every pod
// claimed). Maps to HTTP 429.
func Exhausted(reason string) error {
	return &Error{Sentinel: ErrExhausted, Message: reason}
}

// Unavailable creates an error for an operation whose dependency did not
// answer in time; operation names it (e.g. "kubernetes.getJob"). Maps to
// HTTP 503.
func Unavailable(operation string, cause error) error {
	return &Error{
		Sentinel: ErrUnavailable,
		Message:  fmt.Sprintf("%s did not finish in time: %v", operation, cause),
		Cause:    cause,
	}
}

// Unconfirmed is Unavailable for a write: the request may have reached the
// dependency before the deadline, so the client is told the outcome is
// unknown rather than that nothing happened. Maps to HTTP 503.
func Unconfirmed(operation string, cause error) error {
	return &Error{
		Sentinel: ErrUnavailable,
		Message:  fmt.Sprintf("%s did not finish in time; its outcome is unknown and it may have taken effect: %v", operation, cause),
		Cause:    cause,
	}
}

// Internal creates an internal error wrapping an underlying cause; operation
// names the operation that failed (e.g. "docker.createVolume").
func Internal(operation string, cause error) error {
	return &Error{Sentinel: ErrInternal, Message: fmt.Sprintf("%s: %v", operation, cause), Cause: cause}
}
