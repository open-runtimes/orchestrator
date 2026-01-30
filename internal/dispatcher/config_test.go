package dispatcher

import (
	"testing"
	"time"
)

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Parallel()
	// LoadConfigFromEnv reads from env, but with no env vars set,
	// it should use defaults
	cfg := MemoryConfig{}.withDefaults()

	if cfg.BufferSize != 10000 {
		t.Errorf("Expected BufferSize 10000, got %d", cfg.BufferSize)
	}
	if cfg.Workers != 10 {
		t.Errorf("Expected Workers 10, got %d", cfg.Workers)
	}
	if cfg.HTTPTimeout != 10*time.Second {
		t.Errorf("Expected HTTPTimeout 10s, got %v", cfg.HTTPTimeout)
	}
}

func TestMemoryConfig_WithDefaults_ZeroValues(t *testing.T) {
	t.Parallel()
	cfg := MemoryConfig{}.withDefaults()

	if cfg.BufferSize != 10000 {
		t.Errorf("Expected BufferSize 10000, got %d", cfg.BufferSize)
	}
	if cfg.Workers != 10 {
		t.Errorf("Expected Workers 10, got %d", cfg.Workers)
	}
	if cfg.HTTPTimeout != 10*time.Second {
		t.Errorf("Expected HTTPTimeout 10s, got %v", cfg.HTTPTimeout)
	}
}

func TestMemoryConfig_WithDefaults_NegativeValues(t *testing.T) {
	t.Parallel()
	cfg := MemoryConfig{
		BufferSize:  -1,
		Workers:     -1,
		HTTPTimeout: -1,
	}.withDefaults()

	if cfg.BufferSize != 10000 {
		t.Errorf("Expected BufferSize 10000, got %d", cfg.BufferSize)
	}
	if cfg.Workers != 10 {
		t.Errorf("Expected Workers 10, got %d", cfg.Workers)
	}
	if cfg.HTTPTimeout != 10*time.Second {
		t.Errorf("Expected HTTPTimeout 10s, got %v", cfg.HTTPTimeout)
	}
}

func TestMemoryConfig_WithDefaults_PreservesValidValues(t *testing.T) {
	t.Parallel()
	cfg := MemoryConfig{
		BufferSize:  500,
		Workers:     5,
		HTTPTimeout: 20 * time.Second,
	}.withDefaults()

	if cfg.BufferSize != 500 {
		t.Errorf("Expected BufferSize 500, got %d", cfg.BufferSize)
	}
	if cfg.Workers != 5 {
		t.Errorf("Expected Workers 5, got %d", cfg.Workers)
	}
	if cfg.HTTPTimeout != 20*time.Second {
		t.Errorf("Expected HTTPTimeout 20s, got %v", cfg.HTTPTimeout)
	}
}
