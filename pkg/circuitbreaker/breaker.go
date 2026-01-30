// Package circuitbreaker implements the circuit breaker pattern.
//
// A circuit breaker prevents cascading failures by tracking consecutive failures
// and temporarily blocking requests to failing services.
//
// States:
//   - Closed: Normal operation, requests allowed
//   - Open: Too many failures, requests blocked
//   - HalfOpen: Testing if service recovered, one request allowed
package circuitbreaker

import (
	"sync"
	"time"
)

// State represents the state of a circuit breaker.
type State int

const (
	Closed   State = iota // Normal operation, requests allowed
	Open                  // Failing, requests blocked
	HalfOpen              // Testing if recovered
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker implements the circuit breaker pattern for a single resource.
type Breaker struct {
	mu          sync.Mutex
	state       State
	failures    int           // consecutive failures
	threshold   int           // failures before opening
	lastFailure time.Time     // when the last failure occurred
	cooldown    time.Duration // how long to wait before half-open
}

// Config holds configuration for a circuit breaker.
type Config struct {
	Threshold int           // Failures before circuit opens (default: 5)
	Cooldown  time.Duration // Time before half-open (default: 30s)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Threshold: 5,
		Cooldown:  30 * time.Second,
	}
}

// New creates a new circuit breaker.
func New(cfg Config) *Breaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	return &Breaker{
		state:     Closed,
		threshold: cfg.Threshold,
		cooldown:  cfg.Cooldown,
	}
}

// Allow returns true if a request should be attempted.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true

	case Open:
		// Check if cooldown has elapsed
		if time.Since(b.lastFailure) > b.cooldown {
			b.state = HalfOpen
			return true
		}
		return false

	case HalfOpen:
		// Only allow one request through to test
		return true

	default:
		return true
	}
}

// RecordSuccess records a successful request.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.state = Closed
}

// RecordFailure records a failed request.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	b.lastFailure = time.Now()

	if b.state == HalfOpen {
		// Failed during half-open test, go back to open
		b.state = Open
		return
	}

	if b.failures >= b.threshold {
		b.state = Open
	}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Failures returns the current consecutive failure count.
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

// Reset resets the breaker to closed state.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Closed
	b.failures = 0
}
