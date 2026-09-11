package cloudevent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Sender sends CloudEvents over HTTP.
type Sender struct {
	client *http.Client
}

// NewSender creates a new CloudEvent sender with standard transport settings.
func NewSender(timeout time.Duration) *Sender {
	return &Sender{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Send delivers a CloudEvent via HTTP POST. signingKey, when set, adds an
// HMAC-SHA256 signature of the body in X-Signature-256.
func (s *Sender) Send(ctx context.Context, url string, event *Event, signingKey string) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// CloudEvent headers
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Ce-Specversion", event.SpecVersion)
	req.Header.Set("Ce-Type", event.Type)
	req.Header.Set("Ce-Source", event.Source)
	req.Header.Set("Ce-Subject", event.Subject)
	req.Header.Set("Ce-Id", event.ID)
	req.Header.Set("Ce-Time", event.Time.Format(time.RFC3339))

	if signingKey != "" {
		req.Header.Set("X-Signature-256", generateSignature(body, signingKey))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return &HTTPError{StatusCode: resp.StatusCode}
}

// generateSignature generates HMAC-SHA256 signature.
func generateSignature(payload []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// HTTPError represents an HTTP error response.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// IsClientError returns true for 4xx errors (shouldn't retry).
func IsClientError(err error) bool {
	he := &HTTPError{}
	if errors.As(err, &he) {
		return he.StatusCode >= 400 && he.StatusCode < 500
	}
	return false
}
