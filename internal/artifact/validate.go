package artifact

import (
	"fmt"
	"net/url"
	"orchestrator/internal/apperrors"
	"path/filepath"
	"strings"
)

// Validate validates an artifact at the given index.
func Validate(i int, a Artifact) error {
	field := fmt.Sprintf("artifacts[%d]", i)

	if a.ArtifactID() == "" {
		return apperrors.Validation(field+".id", fmt.Sprintf("artifact[%d]: id is required", i))
	}

	globalRegistryMu.RLock()
	td, ok := globalRegistry[a.ArtifactType()]
	globalRegistryMu.RUnlock()
	if !ok {
		return fmt.Errorf("artifact[%d]: unknown type %q", i, a.ArtifactType())
	}
	if td.Validate == nil {
		return nil
	}
	return td.Validate(field, a)
}

func validateURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("URL must have a host")
	}
	return nil
}

func validatePath(path string) error {
	if path == "" {
		return nil
	}

	if filepath.IsAbs(path) {
		return fmt.Errorf("path must be relative, not absolute")
	}

	cleaned := filepath.Clean(path)
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("path traversal not allowed")
	}

	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal not allowed")
		}
	}

	return nil
}
