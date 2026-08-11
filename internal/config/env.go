package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// GetEnv returns the environment variable value or a default. Values are
// whitespace-trimmed: env vars set via manifests or secret tooling often
// carry a trailing newline, which is never meaningful in a config value.
func GetEnv(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

// GetIntEnv returns an integer environment variable or a default.
func GetIntEnv(key string, defaultValue int) int {
	if intVal, err := strconv.Atoi(GetEnv(key, "")); err == nil {
		return intVal
	}
	return defaultValue
}

// GetFloatEnv returns a float environment variable or a default.
func GetFloatEnv(key string, defaultValue float64) float64 {
	if floatVal, err := strconv.ParseFloat(GetEnv(key, ""), 64); err == nil {
		return floatVal
	}
	return defaultValue
}

// GetDurationEnv returns a duration environment variable or a default.
func GetDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if duration, err := time.ParseDuration(GetEnv(key, "")); err == nil {
		return duration
	}
	return defaultValue
}

// GetBoolEnv returns a boolean environment variable or a default. Accepts
// every strconv.ParseBool form (1/0, t/f, true/false, any casing), falling
// back to the default on absence or a malformed value.
func GetBoolEnv(key string, defaultValue bool) bool {
	if boolVal, err := strconv.ParseBool(GetEnv(key, "")); err == nil {
		return boolVal
	}
	return defaultValue
}

// GetSecretFile reads a secret from a file path.
// Works with Docker secrets (/run/secrets/) and K8s secrets (mounted volumes).
func GetSecretFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
