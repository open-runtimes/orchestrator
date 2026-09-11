package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// GetEnv returns the environment variable value or a default.
func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// GetIntEnv returns an integer environment variable or a default. A value
// that does not parse is logged and treated as unset.
func GetIntEnv(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		slog.Warn("Ignoring malformed environment variable", "key", key, "value", value, "error", err)
		return defaultValue
	}
	return n
}

// GetFloatEnv returns a float environment variable or a default.
func GetFloatEnv(key string, defaultValue float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		slog.Warn("Ignoring malformed environment variable", "key", key, "value", value, "error", err)
		return defaultValue
	}
	return f
}

// GetBoolEnv returns a boolean environment variable or a default.
func GetBoolEnv(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		slog.Warn("Ignoring malformed environment variable", "key", key, "value", value, "error", err)
		return defaultValue
	}
	return b
}

// GetDurationEnv returns a duration environment variable or a default.
func GetDurationEnv(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		slog.Warn("Ignoring malformed environment variable", "key", key, "value", value, "error", err)
		return defaultValue
	}
	return d
}

// GetSecretFile reads a secret from a file path (Docker secrets under
// /run/secrets/, K8s secrets mounted as volumes). An empty path means the
// secret is not configured. An unreadable one is logged loudly: for the API
// key it silently disables authentication otherwise.
func GetSecretFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("Cannot read secret file; treating the secret as unset", "path", path, "error", err)
		return ""
	}
	return strings.TrimSpace(string(data))
}
