// Package apperrors provides structured application errors with HTTP status mapping.
package apperrors

import (
	"errors"
	"fmt"
)

// Sentinel errors for classification via errors.Is().
var (
	ErrValidation = errors.New("validation error")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrInternal   = errors.New("internal error")
)

// Error provides structured error with context.
type Error struct {
	Sentinel error  // Wrapped sentinel for errors.Is() classification
	Message  string // Human-readable message
	Field    string // For validation errors (e.g., "id", "image")
	Resource string // For not found/conflict (e.g., "job")
	Op       string // Operation that failed (e.g., "docker.createVolume")
	Cause    error  // Underlying error
}

// Error returns the human-readable error message.
func (e *Error) Error() string {
	return e.Message
}

// Unwrap returns the sentinel error for errors.Is() classification.
func (e *Error) Unwrap() error {
	return e.Sentinel
}

// Validation creates a validation error for a specific field.
func Validation(field, message string) error {
	return &Error{
		Sentinel: ErrValidation,
		Message:  message,
		Field:    field,
	}
}

// NotFound creates a not found error for a resource.
func NotFound(resource, id string) error {
	return &Error{
		Sentinel: ErrNotFound,
		Message:  fmt.Sprintf("%s %s not found", resource, id),
		Resource: resource,
	}
}

// Conflict creates a conflict error for a resource.
func Conflict(resource, id, reason string) error {
	return &Error{
		Sentinel: ErrConflict,
		Message:  reason,
		Resource: resource,
	}
}

// Internal creates an internal error wrapping an underlying cause.
func Internal(op string, cause error) error {
	return &Error{
		Sentinel: ErrInternal,
		Message:  fmt.Sprintf("%s: %v", op, cause),
		Op:       op,
		Cause:    cause,
	}
}
