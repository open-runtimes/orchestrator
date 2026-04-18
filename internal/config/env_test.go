package config

import (
	"os"
	"testing"
	"time"
)

func TestGetEnv(t *testing.T) {
	// Test default value
	result := GetEnv("TEST_NONEXISTENT_VAR", "default")
	if result != "default" {
		t.Errorf("Expected 'default', got %q", result)
	}

	// Test with set value
	t.Setenv("TEST_GET_ENV", "custom")

	result = GetEnv("TEST_GET_ENV", "default")
	if result != "custom" {
		t.Errorf("Expected 'custom', got %q", result)
	}
}

func TestGetIntEnv(t *testing.T) {
	// Test default value
	result := GetIntEnv("TEST_NONEXISTENT_INT", 42)
	if result != 42 {
		t.Errorf("Expected 42, got %d", result)
	}

	// Test with valid int
	t.Setenv("TEST_INT_ENV", "123")

	result = GetIntEnv("TEST_INT_ENV", 42)
	if result != 123 {
		t.Errorf("Expected 123, got %d", result)
	}

	// Test with invalid int (should return default)
	t.Setenv("TEST_INVALID_INT", "not-a-number")

	result = GetIntEnv("TEST_INVALID_INT", 42)
	if result != 42 {
		t.Errorf("Expected 42 for invalid int, got %d", result)
	}
}

func TestGetDurationEnv(t *testing.T) {
	defaultDuration := 5 * time.Second

	// Test default value
	result := GetDurationEnv("TEST_NONEXISTENT_DURATION", defaultDuration)
	if result != defaultDuration {
		t.Errorf("Expected %v, got %v", defaultDuration, result)
	}

	// Test with valid duration
	t.Setenv("TEST_DURATION_ENV", "30s")

	result = GetDurationEnv("TEST_DURATION_ENV", defaultDuration)
	if result != 30*time.Second {
		t.Errorf("Expected 30s, got %v", result)
	}

	// Test with milliseconds
	t.Setenv("TEST_DURATION_MS", "100ms")

	result = GetDurationEnv("TEST_DURATION_MS", defaultDuration)
	if result != 100*time.Millisecond {
		t.Errorf("Expected 100ms, got %v", result)
	}

	// Test with invalid duration (should return default)
	t.Setenv("TEST_INVALID_DURATION", "not-a-duration")

	result = GetDurationEnv("TEST_INVALID_DURATION", defaultDuration)
	if result != defaultDuration {
		t.Errorf("Expected %v for invalid duration, got %v", defaultDuration, result)
	}
}

func TestGetSecretFile(t *testing.T) {
	// Test empty path
	result := GetSecretFile("")
	if result != "" {
		t.Errorf("Expected empty string for empty path, got %q", result)
	}

	// Test nonexistent file
	result = GetSecretFile("/nonexistent/path/to/secret")
	if result != "" {
		t.Errorf("Expected empty string for nonexistent file, got %q", result)
	}

	// Test with actual file
	tmpFile, err := os.CreateTemp(t.TempDir(), "secret-test")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	secretValue := "my-secret-value"
	if _, err := tmpFile.WriteString(secretValue + "\n"); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	result = GetSecretFile(tmpFile.Name())
	if result != secretValue {
		t.Errorf("Expected %q, got %q", secretValue, result)
	}
}
