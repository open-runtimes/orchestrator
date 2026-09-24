package artifact

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

func validateURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("malformed URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" && scheme != s3Scheme {
		return fmt.Errorf("URL scheme must be http, https, or s3, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("URL must have a host")
	}
	if scheme == s3Scheme && strings.TrimPrefix(parsed.Path, "/") == "" {
		return errors.New("s3 URL must have a key: s3://bucket/key")
	}
	// s3://profile@bucket/key names a configured credential profile; the
	// credentials themselves never ride the URL.
	if _, hasPassword := parsed.User.Password(); scheme == s3Scheme && hasPassword {
		return errors.New("s3 URL must not carry credentials; name a profile as s3://profile@bucket/key")
	}
	return nil
}

// validateGitURL admits the URLs clone can serve: http or https, and no
// userinfo — credentials ride a header, never a string that errors and logs
// echo verbatim.
func validateGitURL(rawURL string) error {
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
	if parsed.User != nil {
		return errors.New("URL must not carry credentials; pass an Authorization header instead")
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

	if slices.Contains(strings.Split(path, "/"), "..") {
		return errors.New("path traversal not allowed")
	}

	return nil
}
