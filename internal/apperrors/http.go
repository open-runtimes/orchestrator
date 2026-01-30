package apperrors

import (
	"errors"
	"net/http"
)

// HTTPStatus maps an error to the appropriate HTTP status code.
func HTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
