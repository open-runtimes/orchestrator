package dispatcher

import (
	"orchestrator/internal/config"
	"time"
)

// Hardcoded delivery defaults - these rarely need tuning.
const (
	defaultMaxRetries       = 3
	defaultInitialBackoff   = 100 * time.Millisecond
	defaultMaxBackoff       = 5 * time.Second
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 30 * time.Second
	defaultMaxRequeues      = 10
)

// Config holds configuration for the in-memory dispatcher.
type Config struct {
	BufferSize      int           // pending events buffer
	Workers         int           // concurrent delivery goroutines
	HTTPTimeout     time.Duration // per-request timeout
	BreakerCooldown time.Duration // circuit breaker cooldown before retry
}

// LoadConfigFromEnv loads dispatcher configuration from environment variables.
func LoadConfigFromEnv() Config {
	return Config{
		BufferSize:      config.GetIntEnv("DISPATCHER_BUFFER_SIZE", 10000),
		Workers:         config.GetIntEnv("DISPATCHER_WORKERS", 10),
		HTTPTimeout:     config.GetDurationEnv("DISPATCHER_HTTP_TIMEOUT", 10*time.Second),
		BreakerCooldown: defaultBreakerCooldown,
	}
}
