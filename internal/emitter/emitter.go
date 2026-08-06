// Package emitter provides a generic fan-out event emitter.
package emitter

// Emitter fans out events of type T to all registered listeners.
type Emitter[T any] struct {
	listeners []func(T)
}

// Register adds a listener. Must be called before the emitter is used.
func (e *Emitter[T]) Register(fn func(T)) {
	e.listeners = append(e.listeners, fn)
}

// Emit sends the event to all registered listeners.
func (e *Emitter[T]) Emit(event T) {
	for _, l := range e.listeners {
		l(event)
	}
}
