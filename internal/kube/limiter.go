package kube

import (
	"context"
	"fmt"

	"k8s.io/client-go/util/flowcontrol"
)

// deadlineLimiter is a client-go rate limiter whose up-front refusal of a wait
// that would outlast the caller's deadline reports context.DeadlineExceeded,
// as the wait running out would have, so callers classify both alike.
type deadlineLimiter struct {
	flowcontrol.RateLimiter
}

func (l deadlineLimiter) Wait(ctx context.Context) error {
	err := l.RateLimiter.Wait(ctx)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return err
	}
	return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
}
