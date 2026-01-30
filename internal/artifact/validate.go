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

	switch art := a.(type) {
	case *Download:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (url) is required", i))
		}
		if err := validateURL(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (url): %v", i, err))
		}
		if art.Out == "" {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: out (path) is required", i))
		}
		if err := validatePath(art.Out); err != nil {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: invalid out (path): %v", i, err))
		}

	case *Upload:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (path) is required", i))
		}
		if err := validatePath(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (path): %v", i, err))
		}
		if art.Out == "" {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: out (url) is required", i))
		}
		if err := validateURL(art.Out); err != nil {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: invalid out (url): %v", i, err))
		}

	case *Write:
		if art.Out == "" {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: out (path) is required", i))
		}
		if err := validatePath(art.Out); err != nil {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: invalid out (path): %v", i, err))
		}
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (content) is required", i))
		}

	case *Read:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (path) is required", i))
		}
		if err := validatePath(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (path): %v", i, err))
		}

	case *Archive:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (path) is required", i))
		}
		if err := validatePath(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (path): %v", i, err))
		}
		if art.Out == "" {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: out (dest) is required", i))
		}
		if err := validatePath(art.Out); err != nil {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: invalid out (dest): %v", i, err))
		}
		if art.Format != "tar.gz" {
			return apperrors.Validation(field+".format", fmt.Sprintf("artifact[%d]: format must be \"tar.gz\"", i))
		}

	case *Unarchive:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (path) is required", i))
		}
		if err := validatePath(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (path): %v", i, err))
		}
		if art.Out == "" {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: out (dest) is required", i))
		}
		if err := validatePath(art.Out); err != nil {
			return apperrors.Validation(field+".out", fmt.Sprintf("artifact[%d]: invalid out (dest): %v", i, err))
		}

	case *List:
		if art.In == "" {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: in (path) is required", i))
		}
		if err := validatePath(art.In); err != nil {
			return apperrors.Validation(field+".in", fmt.Sprintf("artifact[%d]: invalid in (path): %v", i, err))
		}
	}

	return nil
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
