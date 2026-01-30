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

// MemoryConfig holds configuration for the in-memory dispatcher.
type MemoryConfig struct {
	BufferSize  int           // pending events buffer (default: 10000)
	Workers     int           // concurrent delivery goroutines (default: 10)
	HTTPTimeout time.Duration // per-request timeout (default: 10s)
}

// LoadConfigFromEnv loads dispatcher configuration from environment variables.
func LoadConfigFromEnv() MemoryConfig {
	cfg := MemoryConfig{
		BufferSize:  config.GetIntEnv("DISPATCHER_BUFFER_SIZE", 10000),
		Workers:     config.GetIntEnv("DISPATCHER_WORKERS", 10),
		HTTPTimeout: config.GetDurationEnv("DISPATCHER_HTTP_TIMEOUT", 10*time.Second),
	}
	return cfg.withDefaults()
}

// withDefaults fills in zero values with defaults.
func (c MemoryConfig) withDefaults() MemoryConfig {
	if c.BufferSize <= 0 {
		c.BufferSize = 10000
	}
	if c.Workers <= 0 {
		c.Workers = 10
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 10 * time.Second
	}
	return c
}
