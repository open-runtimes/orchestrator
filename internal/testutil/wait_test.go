package testutil

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitFor_ImmediateSuccess(t *testing.T) {
	t.Parallel()
	result := WaitFor(t, func() bool {
		return true
	}, WithTimeout(time.Second))

	if !result {
		t.Error("expected WaitFor to return true for immediate success")
	}
}

func TestWaitFor_EventualSuccess(t *testing.T) {
	t.Parallel()
	counter := 0
	result := WaitFor(t, func() bool {
		counter++
		return counter >= 3
	}, WithTimeout(time.Second), WithInterval(10*time.Millisecond))

	if !result {
		t.Error("expected WaitFor to return true for eventual success")
	}
	if counter < 3 {
		t.Errorf("expected counter >= 3, got %d", counter)
	}
}

func TestWaitFor_Timeout(t *testing.T) {
	t.Parallel()
	result := WaitFor(t, func() bool {
		return false
	}, WithTimeout(50*time.Millisecond), WithInterval(10*time.Millisecond))

	if result {
		t.Error("expected WaitFor to return false on timeout")
	}
}

func TestWaitForCount_Success(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(10 * time.Millisecond)
			counter.Add(1)
		}
	}()

	result := WaitForCount(t, &counter, 5, WithTimeout(time.Second), WithInterval(10*time.Millisecond))

	if !result {
		t.Error("expected WaitForCount to return true")
	}
}

func TestWaitForCount_Timeout(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	counter.Store(2)

	result := WaitForCount(t, &counter, 10, WithTimeout(50*time.Millisecond), WithInterval(10*time.Millisecond))

	if result {
		t.Error("expected WaitForCount to return false on timeout")
	}
}

func TestMustWaitFor_Success(t *testing.T) {
	t.Parallel()
	// Should not panic or fail
	MustWaitFor(t, func() bool {
		return true
	}, WithTimeout(time.Second))
}

func TestMustWaitForCount_Success(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	counter.Store(5)

	// Should not panic or fail
	MustWaitForCount(t, &counter, 5, WithTimeout(time.Second))
}

func TestWithTimeout(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	WithTimeout(5 * time.Second)(&opts)

	if opts.Timeout != 5*time.Second {
		t.Errorf("expected Timeout to be 5s, got %v", opts.Timeout)
	}
}

func TestWithInterval(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	WithInterval(50 * time.Millisecond)(&opts)

	if opts.Interval != 50*time.Millisecond {
		t.Errorf("expected Interval to be 50ms, got %v", opts.Interval)
	}
}

func TestDefaultOptions(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()

	if opts.Timeout != 30*time.Second {
		t.Errorf("expected default Timeout to be 30s, got %v", opts.Timeout)
	}
	if opts.Interval != 100*time.Millisecond {
		t.Errorf("expected default Interval to be 100ms, got %v", opts.Interval)
	}
}
