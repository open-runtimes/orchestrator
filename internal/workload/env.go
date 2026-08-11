package workload

import (
	"fmt"
	"strings"

	"orchestrator/internal/apperrors"
)

// NormalizeEnv trims whitespace from environment variable names in place.
// Values are strings by type (a non-string is rejected at decode) and are
// passed through untouched — whitespace may be meaningful there. A name that
// is empty or collides with another after trimming is a validation error:
// silently merging two entries would drop one of their values.
func NormalizeEnv(env map[string]string) error {
	for name, value := range env {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return apperrors.Validation("environment", "environment variable name must not be blank")
		}
		if trimmed == name {
			continue
		}
		if _, exists := env[trimmed]; exists {
			return apperrors.Validation("environment", fmt.Sprintf("environment variable name %q collides with %q after trimming", name, trimmed))
		}
		delete(env, name)
		env[trimmed] = value
	}
	return nil
}
