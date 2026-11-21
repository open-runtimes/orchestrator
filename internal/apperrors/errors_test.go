package apperrors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestValidation(t *testing.T) {
	t.Parallel()
	err := Validation("id", "job ID is required")

	if !errors.Is(err, ErrValidation) {
		t.Error("expected error to match ErrValidation")
	}
	if err.Error() != "job ID is required" {
		t.Errorf("expected message 'job ID is required', got %q", err.Error())
	}

	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatal("expected error to be *Error")
	}
	if appErr.Field != "id" {
		t.Errorf("expected field 'id', got %q", appErr.Field)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	err := NotFound("job", "abc123")

	if !errors.Is(err, ErrNotFound) {
		t.Error("expected error to match ErrNotFound")
	}
	if err.Error() != "job abc123 not found" {
		t.Errorf("expected message 'job abc123 not found', got %q", err.Error())
	}
}

func TestConflict(t *testing.T) {
	t.Parallel()
	err := Conflict("job already exists")

	if !errors.Is(err, ErrConflict) {
		t.Error("expected error to match ErrConflict")
	}
	if err.Error() != "job already exists" {
		t.Errorf("expected message 'job already exists', got %q", err.Error())
	}
}

func TestExhausted(t *testing.T) {
	t.Parallel()
	err := Exhausted("pool std has no free warm pod")

	if !errors.Is(err, ErrExhausted) {
		t.Error("expected error to match ErrExhausted")
	}
	if err.Error() != "pool std has no free warm pod" {
		t.Errorf("expected message 'pool std has no free warm pod', got %q", err.Error())
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()
	err := Unavailable("kubernetes.getJob", context.DeadlineExceeded)

	if !errors.Is(err, ErrUnavailable) {
		t.Error("expected error to match ErrUnavailable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("expected cause to be preserved")
	}
	if want := "kubernetes.getJob did not finish in time: context deadline exceeded"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if got := HTTPStatus(fmt.Errorf("wrap: %w", err)); got != http.StatusServiceUnavailable {
		t.Errorf("HTTPStatus() = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestUnconfirmed(t *testing.T) {
	t.Parallel()
	err := Unconfirmed("kubernetes.createJob", context.DeadlineExceeded)

	if !errors.Is(err, ErrUnavailable) {
		t.Error("expected error to match ErrUnavailable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("expected cause to be preserved")
	}
	if !strings.Contains(err.Error(), "outcome is unknown") {
		t.Errorf("message must say the outcome is unknown, got %q", err.Error())
	}
	if got := HTTPStatus(fmt.Errorf("wrap: %w", err)); got != http.StatusServiceUnavailable {
		t.Errorf("HTTPStatus() = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestInternal(t *testing.T) {
	t.Parallel()
	cause := errors.New("docker daemon unavailable")
	err := Internal("docker.createVolume", cause)

	if !errors.Is(err, ErrInternal) {
		t.Error("expected error to match ErrInternal")
	}
	if err.Error() != "docker.createVolume: docker daemon unavailable" {
		t.Errorf("unexpected message: %q", err.Error())
	}

	if !errors.Is(err, cause) {
		t.Error("expected cause to be preserved")
	}
}

func TestHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{"validation", Validation("id", "required"), http.StatusBadRequest},
		{"not found", NotFound("job", "123"), http.StatusNotFound},
		{"conflict", Conflict("exists"), http.StatusConflict},
		{"exhausted", Exhausted("no free warm pod"), http.StatusTooManyRequests},
		{"unavailable", Unavailable("op", context.DeadlineExceeded), http.StatusServiceUnavailable},
		{"unconfirmed", Unconfirmed("op", context.DeadlineExceeded), http.StatusServiceUnavailable},
		{"internal", Internal("op", errors.New("fail")), http.StatusInternalServerError},
		{"sentinel validation", ErrValidation, http.StatusBadRequest},
		{"sentinel not found", ErrNotFound, http.StatusNotFound},
		{"sentinel conflict", ErrConflict, http.StatusConflict},
		{"sentinel exhausted", ErrExhausted, http.StatusTooManyRequests},
		{"sentinel unavailable", ErrUnavailable, http.StatusServiceUnavailable},
		{"sentinel internal", ErrInternal, http.StatusInternalServerError},
		{"wrapped validation", fmt.Errorf("wrap: %w", Validation("f", "m")), http.StatusBadRequest},
		{"unknown error", errors.New("unknown"), http.StatusInternalServerError},
		{"nil error", nil, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := HTTPStatus(tt.err)
			if got != tt.expected {
				t.Errorf("HTTPStatus() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestErrorsIsWithWrapping(t *testing.T) {
	t.Parallel()
	// Ensure errors.Is works through fmt.Errorf wrapping
	original := Validation("id", "required")
	wrapped := fmt.Errorf("service error: %w", original)
	doubleWrapped := fmt.Errorf("handler error: %w", wrapped)

	if !errors.Is(doubleWrapped, ErrValidation) {
		t.Error("expected errors.Is to find ErrValidation through multiple wraps")
	}
}
