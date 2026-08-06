// Package backoff provides exponential backoff calculation.
package backoff

import (
	"math"
	"time"
)

// Config for exponential backoff. Zero values use defaults.
type Config struct {
	Initial time.Duration // default: 100ms
	Max     time.Duration // default: 5s
}

// Exponential calculates exponential backoff for a given attempt.
// Attempt 1 returns initial, attempt 2 returns initial*2, etc.
func Exponential(attempt int, cfg *Config) time.Duration {
	initial := 100 * time.Millisecond
	maxBackoff := 5 * time.Second
	if cfg != nil {
		if cfg.Initial > 0 {
			initial = cfg.Initial
		}
		if cfg.Max > 0 {
			maxBackoff = cfg.Max
		}
	}

	if attempt < 1 {
		return initial
	}
	backoff := float64(initial) * math.Pow(2.0, float64(attempt-1))
	if backoff > float64(maxBackoff) {
		backoff = float64(maxBackoff)
	}
	return time.Duration(backoff)
}
