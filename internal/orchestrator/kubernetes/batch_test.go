package kubernetes

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// batchCollector runs batchLines in the background and collects emitted
// batches; done closes when batchLines returns.
type batchCollector struct {
	mu      sync.Mutex
	batches [][]string
	done    chan struct{}
}

func collectBatches(lines <-chan string, flushWait time.Duration, maxBatch int) *batchCollector {
	c := &batchCollector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		batchLines(lines, flushWait, maxBatch, func(batch []string) {
			c.mu.Lock()
			c.batches = append(c.batches, slices.Clone(batch))
			c.mu.Unlock()
		})
	}()
	return c
}

func (c *batchCollector) snapshot() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.batches)
}

func (c *batchCollector) waitForBatches(t *testing.T, n int) [][]string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := c.snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d batches, have %v", n, c.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A partial batch must flush on the tick — not wait for the stream to end.
// This is the live-log guarantee: a quiet build's few lines still reach the
// callback within a flush interval.
func TestBatchLinesFlushesOnTick(t *testing.T) {
	lines := make(chan string, 8)
	c := collectBatches(lines, 20*time.Millisecond, 32)

	lines <- "one"
	lines <- "two"
	got := c.waitForBatches(t, 1)
	if want := []string{"one", "two"}; !slices.Equal(got[0], want) {
		t.Errorf("first batch = %v, want %v", got[0], want)
	}

	lines <- "three"
	got = c.waitForBatches(t, 2)
	if want := []string{"three"}; !slices.Equal(got[1], want) {
		t.Errorf("second batch = %v, want %v", got[1], want)
	}

	close(lines)
	<-c.done
}

// A full batch must flush immediately without waiting for the tick.
func TestBatchLinesFlushesAtMaxBatch(t *testing.T) {
	lines := make(chan string, 8)
	c := collectBatches(lines, time.Hour, 2)

	lines <- "one"
	lines <- "two"
	got := c.waitForBatches(t, 1)
	if want := []string{"one", "two"}; !slices.Equal(got[0], want) {
		t.Errorf("batch = %v, want %v", got[0], want)
	}

	close(lines)
	<-c.done
}

// Whatever is buffered when the stream ends must flush before returning.
func TestBatchLinesFlushesRemainderOnClose(t *testing.T) {
	lines := make(chan string, 8)
	c := collectBatches(lines, time.Hour, 32)

	lines <- "tail"
	close(lines)
	<-c.done

	got := c.snapshot()
	if len(got) != 1 || !slices.Equal(got[0], []string{"tail"}) {
		t.Errorf("batches = %v, want [[tail]]", got)
	}
}

// Idle ticks with an empty buffer must not emit empty batches.
func TestBatchLinesSkipsEmptyTicks(t *testing.T) {
	lines := make(chan string)
	c := collectBatches(lines, 5*time.Millisecond, 32)

	time.Sleep(30 * time.Millisecond)
	if got := c.snapshot(); len(got) != 0 {
		t.Errorf("batches = %v, want none for an idle stream", got)
	}

	close(lines)
	<-c.done
}
