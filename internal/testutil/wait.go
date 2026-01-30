// Package testutil provides testing utilities for polling and waiting.
package testutil

import (
	"sync/atomic"
	"testing"
	"time"
)

// WaitOptions configures WaitFor behavior.
type WaitOptions struct {
	Timeout  time.Duration
	Interval time.Duration
}

// WaitOption is a functional option for WaitFor.
type WaitOption func(*WaitOptions)

// WithTimeout sets the maximum wait time (default: 30s).
func WithTimeout(d time.Duration) WaitOption {
	return func(o *WaitOptions) {
		o.Timeout = d
	}
}

// WithInterval sets the polling interval (default: 100ms).
func WithInterval(d time.Duration) WaitOption {
	return func(o *WaitOptions) {
		o.Interval = d
	}
}

func defaultOptions() WaitOptions {
	return WaitOptions{
		Timeout:  30 * time.Second,
		Interval: 100 * time.Millisecond,
	}
}

// WaitFor polls until condition returns true or timeout is reached.
// Returns true if condition was met, false on timeout.
func WaitFor(tb testing.TB, condition func() bool, opts ...WaitOption) bool {
	tb.Helper()

	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	deadline := time.Now().Add(o.Timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(o.Interval)
	}
	return false
}

// WaitForCount polls until counter reaches the target value or timeout is reached.
// Returns true if target was reached, false on timeout.
func WaitForCount(tb testing.TB, counter *atomic.Int64, target int64, opts ...WaitOption) bool {
	tb.Helper()
	return WaitFor(tb, func() bool {
		return counter.Load() >= target
	}, opts...)
}

// MustWaitFor polls until condition returns true or fails the test on timeout.
func MustWaitFor(tb testing.TB, condition func() bool, opts ...WaitOption) {
	tb.Helper()
	if !WaitFor(tb, condition, opts...) {
		tb.Fatal("timed out waiting for condition")
	}
}

// MustWaitForCount polls until counter reaches the target value or fails the test on timeout.
func MustWaitForCount(tb testing.TB, counter *atomic.Int64, target int64, opts ...WaitOption) {
	tb.Helper()
	if !WaitForCount(tb, counter, target, opts...) {
		tb.Fatalf("timed out waiting for counter to reach %d (current: %d)", target, counter.Load())
	}
}
