// Package backoff provides exponential backoff calculation.
package backoff

import (
	"cmp"
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
func Exponential(attempt int, cfg Config) time.Duration {
	initial := cmp.Or(cfg.Initial, 100*time.Millisecond)
	maxBackoff := cmp.Or(cfg.Max, 5*time.Second)

	if attempt < 1 {
		return initial
	}
	backoff := float64(initial) * math.Pow(2.0, float64(attempt-1))
	if backoff > float64(maxBackoff) {
		backoff = float64(maxBackoff)
	}
	return time.Duration(backoff)
}
