//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/api"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
	"orchestrator/internal/orchestrator/docker"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/cloudevent"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkConcurrentJobs stress tests the system with concurrent job creation and callbacks.
// Run with: go test -tags=e2e -run=^$ -bench=BenchmarkConcurrentJobs -benchtime=30s ./e2e/
func BenchmarkConcurrentJobs(b *testing.B) {
	var callbackCount atomic.Int64
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbackCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	server, cleanup := createBenchServer(b)
	defer cleanup()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		client := &http.Client{Timeout: 30 * time.Second}
		i := 0
		for pb.Next() {
			i++
			jobID := fmt.Sprintf("bench-%d-%d", time.Now().UnixNano(), i)

			req := job.Request{
				ID:             jobID,
				Image:          "alpine:latest",
				Command:        "echo hello",
				TimeoutSeconds: 30,
				Callback: &job.Callback{
					URL: callbackServer.URL,
				},
			}

			body, _ := json.Marshal(req)
			resp, err := client.Post(server+"/v1/jobs", "application/json", bytes.NewReader(body))
			if err != nil {
				b.Errorf("Failed to create job: %v", err)
				continue
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				b.Errorf("Expected 202, got %d", resp.StatusCode)
			}
		}
	})

	b.StopTimer()
	b.ReportMetric(float64(callbackCount.Load()), "callbacks")

	if callbackCount.Load() == 0 {
		b.Error("Expected at least some callbacks to be received")
	}
}

// TestCallbackThroughput measures how many callbacks the dispatcher can handle.
func TestCallbackThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping throughput test in short mode")
	}

	const (
		numCallbacks    = 10000
		concurrency     = 100
		callbackTimeout = 30 * time.Second
	)

	var received atomic.Int64
	var totalLatency atomic.Int64
	startTime := time.Now()

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		latency := time.Since(startTime).Microseconds()
		totalLatency.Add(latency)
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{
		BufferSize:  numCallbacks,
		Workers:     concurrency,
		HTTPTimeout: 5 * time.Second,
	}, nil)
	defer d.Close(context.Background())

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)

	dispatchStart := time.Now()
	for i := 0; i < numCallbacks; i++ {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(id int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			event := &dispatcher.Event{
				Payload:     newTestEvent(fmt.Sprintf("event-%d", id)),
				Destination: callbackServer.URL,
			}
			if err := d.Dispatch(event); err != nil {
				t.Logf("Dispatch error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	dispatchDuration := time.Since(dispatchStart)

	testutil.WaitForCount(t, &received, numCallbacks, testutil.WithTimeout(callbackTimeout))
	totalDuration := time.Since(dispatchStart)

	stats := d.Stats()
	receivedCount := received.Load()
	avgLatency := float64(totalLatency.Load()) / float64(receivedCount) / 1000.0

	t.Logf("=== Callback Throughput Test ===")
	t.Logf("Dispatched:    %d events in %v", numCallbacks, dispatchDuration)
	t.Logf("Dispatch rate: %.0f events/sec", float64(numCallbacks)/dispatchDuration.Seconds())
	t.Logf("Received:      %d/%d callbacks", receivedCount, numCallbacks)
	t.Logf("Delivered:     %d", stats.Delivered)
	t.Logf("Failed:        %d", stats.Failed)
	t.Logf("Dropped:       %d", stats.Dropped)
	t.Logf("Total time:    %v", totalDuration)
	t.Logf("Throughput:    %.0f callbacks/sec", float64(receivedCount)/totalDuration.Seconds())
	t.Logf("Avg latency:   %.2f ms", avgLatency)

	if receivedCount < int64(numCallbacks*0.99) {
		t.Errorf("Expected at least 99%% delivery, got %.1f%%", float64(receivedCount)/float64(numCallbacks)*100)
	}
}

// TestConcurrentJobsWithCallbacks creates many jobs concurrently with callbacks.
func TestConcurrentJobsWithCallbacks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent jobs test in short mode")
	}

	const (
		numJobs     = 50
		concurrency = 10
		jobTimeout  = 60 * time.Second
	)

	var callbacks sync.Map
	incCallback := func(eventType string) {
		val, _ := callbacks.LoadOrStore(eventType, new(atomic.Int64))
		val.(*atomic.Int64).Add(1)
	}

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		if err := json.NewDecoder(r.Body).Decode(&event); err == nil {
			if t, ok := event["type"].(string); ok {
				// Only count start and exit events (ignore log events)
				if t == "orchestrator.job.start" || t == "orchestrator.job.exit" {
					incCallback(t)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	server, cleanup := createBenchServer(t)
	defer cleanup()

	client := &http.Client{Timeout: jobTimeout}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	var created, failed atomic.Int64

	start := time.Now()
	for i := 0; i < numJobs; i++ {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(id int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			jobID := fmt.Sprintf("concurrent-%d-%d", time.Now().UnixNano(), id)
			req := job.Request{
				ID:             jobID,
				Image:          "alpine:latest",
				Command:        "sh -c 'echo start; sleep 0.1; echo done'",
				TimeoutSeconds: 30,
				Callback: &job.Callback{
					URL: callbackServer.URL,
					// Empty Events means all events are sent
				},
			}

			body, _ := json.Marshal(req)
			resp, err := client.Post(server+"/v1/jobs", "application/json", bytes.NewReader(body))
			if err != nil {
				failed.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusAccepted {
				created.Add(1)
			} else {
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	createDuration := time.Since(start)

	t.Log("Waiting for jobs to complete...")
	// Wait for callbacks - expect at least start+exit per created job
	expectedCallbacks := created.Load() * 2
	testutil.WaitFor(t, func() bool {
		var total int64
		callbacks.Range(func(_, value any) bool {
			total += value.(*atomic.Int64).Load()
			return true
		})
		return total >= expectedCallbacks
	}, testutil.WithTimeout(60*time.Second))

	t.Logf("=== Concurrent Jobs Test ===")
	t.Logf("Jobs created:  %d/%d in %v", created.Load(), numJobs, createDuration)
	t.Logf("Jobs failed:   %d", failed.Load())
	t.Logf("Create rate:   %.1f jobs/sec", float64(created.Load())/createDuration.Seconds())
	t.Log("Callbacks received:")
	callbacks.Range(func(key, value any) bool {
		t.Logf("  %s: %d", key, value.(*atomic.Int64).Load())
		return true
	})

	// Assertions
	if created.Load() < int64(numJobs*0.9) {
		t.Errorf("Expected at least 90%% job creation success, got %d/%d", created.Load(), numJobs)
	}

	// Check we received start and exit callbacks for most jobs
	var startCallbacks, exitCallbacks int64
	if v, ok := callbacks.Load("orchestrator.job.start"); ok {
		startCallbacks = v.(*atomic.Int64).Load()
	}
	if v, ok := callbacks.Load("orchestrator.job.exit"); ok {
		exitCallbacks = v.(*atomic.Int64).Load()
	}

	if startCallbacks < created.Load() {
		t.Errorf("Expected 100%% start callbacks, got %d/%d", startCallbacks, created.Load())
	}
	if exitCallbacks < created.Load() {
		t.Errorf("Expected 100%% exit callbacks, got %d/%d", exitCallbacks, created.Load())
	}
}

// TestDispatcherUnderLoad tests dispatcher behavior under extreme load.
func TestDispatcherUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	const (
		eventRate     = 1000 // events per second target
		duration      = 10   // seconds
		totalEvents   = eventRate * duration
		slowPercent   = 5   // percentage of slow callbacks
		slowLatencyMs = 500 // latency for slow callbacks
	)

	var received, slow atomic.Int64

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if received.Add(1)%int64(100/slowPercent) == 0 {
			slow.Add(1)
			time.Sleep(time.Duration(slowLatencyMs) * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{
		BufferSize:  totalEvents,
		Workers:     50,
		HTTPTimeout: 2 * time.Second,
	}, nil)
	defer d.Close(context.Background())

	ticker := time.NewTicker(time.Second / time.Duration(eventRate))
	defer ticker.Stop()

	start := time.Now()
	var dispatched atomic.Int64

	go func() {
		for i := 0; i < totalEvents; i++ {
			<-ticker.C
			event := &dispatcher.Event{
				Payload:     newTestEvent(fmt.Sprintf("load-%d", i)),
				Destination: callbackServer.URL,
			}
			if err := d.Dispatch(event); err == nil {
				dispatched.Add(1)
			}
		}
	}()

	// Wait for all events to be dispatched, then wait for delivery
	testutil.WaitFor(t, func() bool {
		return dispatched.Load() >= int64(totalEvents)
	}, testutil.WithTimeout(time.Duration(duration+5)*time.Second))

	// Wait for delivery to complete
	testutil.WaitFor(t, func() bool {
		stats := d.Stats()
		return stats.Delivered+stats.Failed+stats.Dropped >= dispatched.Load()
	}, testutil.WithTimeout(10*time.Second))

	stats := d.Stats()
	elapsed := time.Since(start)

	t.Logf("=== Dispatcher Load Test ===")
	t.Logf("Target rate:   %d events/sec for %ds", eventRate, duration)
	t.Logf("Dispatched:    %d events", dispatched.Load())
	t.Logf("Received:      %d callbacks", received.Load())
	t.Logf("Slow calls:    %d (%.1f%%)", slow.Load(), float64(slow.Load())/float64(received.Load())*100)
	t.Logf("Delivered:     %d", stats.Delivered)
	t.Logf("Failed:        %d", stats.Failed)
	t.Logf("Dropped:       %d", stats.Dropped)
	t.Logf("Retries:       %d", stats.RetriesTotal)
	t.Logf("Requeued:      %d", stats.Requeued)
	t.Logf("Elapsed:       %v", elapsed)
	t.Logf("Actual rate:   %.0f events/sec", float64(received.Load())/elapsed.Seconds())

	// Assertions
	dispatchedCount := dispatched.Load()
	receivedCount := received.Load()

	if dispatchedCount < int64(totalEvents*0.9) {
		t.Errorf("Expected to dispatch at least 90%% of events, got %d/%d", dispatchedCount, totalEvents)
	}

	deliveryRate := float64(receivedCount) / float64(dispatchedCount) * 100
	if deliveryRate < 90 {
		t.Errorf("Expected at least 90%% delivery rate, got %.1f%%", deliveryRate)
	}

	if stats.Dropped > int64(totalEvents*0.05) {
		t.Errorf("Too many dropped events: %d (max 5%% of %d)", stats.Dropped, totalEvents)
	}
}

func createBenchServer(tb testing.TB) (string, func()) {
	// If E2E_API_URL is set, use external server
	if url := os.Getenv("E2E_API_URL"); url != "" {
		tb.Logf("Using external API: %s", url)
		return url, func() {}
	}

	ctx := context.Background()

	metrics, _, err := observability.NewMetrics(ctx)
	if err != nil {
		tb.Fatalf("Failed to create metrics: %v", err)
	}

	eventDispatcher := dispatcher.NewMemory(dispatcher.MemoryConfig{
		BufferSize:  10000,
		Workers:     50,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	// Create unstarted server to get the listener port
	tempServer := httptest.NewUnstartedServer(nil)
	port := tempServer.Listener.Addr().(*net.TCPAddr).Port
	callbackProxyURL := fmt.Sprintf("http://host.docker.internal:%d", port)

	orchestrator, err := docker.NewOrchestrator(ctx, docker.Config{
		SidecarImage:        "ko.local/job-sidecar:latest",
		RetentionPeriod:     5 * time.Minute,
		MaintenanceInterval: 1 * time.Minute,
		Dispatcher:          eventDispatcher,
		CallbackProxyURL:    callbackProxyURL,
	})
	if err != nil {
		tempServer.Close()
		tb.Fatalf("Failed to create orchestrator: %v", err)
	}

	svc := job.NewService(orchestrator, metrics)
	healthChecker := health.NewChecker(orchestrator)

	router := api.NewRouter(api.RouterConfig{
		JobService:    svc,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		Dispatcher:    eventDispatcher,
	})

	// Assign the router and start the server
	tempServer.Config.Handler = router
	tempServer.Start()

	cleanup := func() {
		tempServer.Close()
		eventDispatcher.Close(context.Background())
		orchestrator.Close()
	}

	return tempServer.URL, cleanup
}

func newTestEvent(id string) *cloudevent.CloudEvent {
	return cloudevent.New("test.benchmark", "benchmark", "test", id, map[string]any{"test": true})
}
