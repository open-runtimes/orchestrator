package circuitbreaker

import (
	"sync"
)

// Registry manages circuit breakers for multiple resources.
// Breakers are created lazily on first access.
type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	config   Config
}

// NewRegistry creates a new registry with the given default config.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		breakers: make(map[string]*Breaker),
		config:   cfg,
	}
}

// Get returns the circuit breaker for a key, creating one if needed.
func (r *Registry) Get(key string) *Breaker {
	r.mu.RLock()
	b, exists := r.breakers[key]
	r.mu.RUnlock()

	if exists {
		return b
	}

	// Create new breaker
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if b, exists = r.breakers[key]; exists {
		return b
	}

	b = New(r.config)
	r.breakers[key] = b
	return b
}

// Stats returns statistics about the registry.
func (r *Registry) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := Stats{
		Total: len(r.breakers),
	}
	for _, b := range r.breakers {
		switch b.State() {
		case Open:
			stats.Open++
		case HalfOpen:
			stats.HalfOpen++
		case Closed:
			stats.Closed++
		}
	}
	return stats
}

// Stats holds registry statistics.
type Stats struct {
	Total    int // Total breakers
	Open     int // Breakers in open state
	HalfOpen int // Breakers in half-open state
	Closed   int // Breakers in closed state
}

// Reset resets all breakers in the registry.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, b := range r.breakers {
		b.Reset()
	}
}

// Remove removes a breaker from the registry.
func (r *Registry) Remove(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breakers, key)
}

// Keys returns all registered keys.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.breakers))
	for k := range r.breakers {
		keys = append(keys, k)
	}
	return keys
}
