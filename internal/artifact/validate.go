package artifact

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Validate validates an artifact at the given index using the default registry.
func Validate(i int, a Artifact) error {
	return DefaultRegistry().Validate(i, a)
}

func validateURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("malformed URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("URL must have a host")
	}
	return nil
}

func validatePath(path string) error {
	if path == "" {
		return nil
	}

	if filepath.IsAbs(path) {
		return errors.New("path must be relative, not absolute")
	}

	cleaned := filepath.Clean(path)
	if strings.HasPrefix(cleaned, "..") {
		return errors.New("path traversal not allowed")
	}

	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return errors.New("path traversal not allowed")
		}
	}

	return nil
}
