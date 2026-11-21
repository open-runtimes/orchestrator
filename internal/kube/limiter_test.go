package kube

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewConfig_RefusedWaitReportsTheDeadline(t *testing.T) {
	config := newTestConfig(t, 1, 1)
	drain(config.RateLimiter)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := config.RateLimiter.Wait(ctx)
	if err == nil {
		t.Fatal("Wait() with a 50ms deadline on an empty 1 QPS bucket returned nil, want a refusal")
	}
	if ctx.Err() != nil {
		t.Fatalf("Wait() returned only once the deadline passed (%v), want an up-front refusal", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait() refusal = %v, want it to match context.DeadlineExceeded", err)
	}
}

func TestNewConfig_CancelledWaitIsNotADeadline(t *testing.T) {
	config := newTestConfig(t, 1, 1)
	drain(config.RateLimiter)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := config.RateLimiter.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait() on a cancelled context = %v, want context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait() on a cancelled context without a deadline = %v, must not match context.DeadlineExceeded", err)
	}
}
