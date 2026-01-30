//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/api"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/orchestrator/docker"
	"orchestrator/internal/testutil"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// getTestURL returns the base URL for e2e tests.
// If E2E_API_URL is set, tests run against that instance.
// Otherwise, a test server is created.
func getTestURL(t *testing.T) (string, func()) {
	if url := os.Getenv("E2E_API_URL"); url != "" {
		t.Logf("Using external API: %s", url)
		return url, func() {}
	}

	server, _, cleanup := createTestServer(t)
	return server.URL, cleanup
}

func createTestServer(t *testing.T) (*httptest.Server, *job.Service, func()) {
	eventDispatcher := dispatcher.NewMemory(dispatcher.MemoryConfig{
		BufferSize: 100,
		Workers:    2,
	}, nil)

	orchestrator, err := docker.NewOrchestrator(context.Background(), docker.Config{
		SidecarImage: "ko.local/job-sidecar:latest",
		Dispatcher:   eventDispatcher,
	})
	if err != nil {
		t.Fatalf("Failed to create Docker orchestrator: %v", err)
	}

	svc := job.NewService(orchestrator, nil)
	healthChecker := health.NewChecker(orchestrator)

	router := api.NewRouter(api.RouterConfig{
		JobService:    svc,
		HealthChecker: healthChecker,
		Dispatcher:    eventDispatcher,
	})

	server := httptest.NewServer(router)

	cleanup := func() {
		orchestrator.Close()
		// Drain dispatcher before closing server so pending callbacks can be delivered
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		eventDispatcher.Close(ctx)
		server.Close()
	}

	return server, svc, cleanup
}

func TestAPI_Readyz(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	resp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var result health.Response
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Status != health.StatusHealthy {
		t.Errorf("Expected healthy status, got %s", result.Status)
	}
}

func TestAPI_Livez(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	resp, err := http.Get(baseURL + "/livez")
	if err != nil {
		t.Fatalf("Liveness check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestAPI_CreateAndGetJob(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	jobID := fmt.Sprintf("e2e-test-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "echo 'hello' && sleep 5",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Create job failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}

	var createResp map[string]string
	json.NewDecoder(resp.Body).Decode(&createResp)

	if createResp["id"] != jobID {
		t.Errorf("Expected job ID %s, got %s", jobID, createResp["id"])
	}

	if createResp["status"] != "accepted" {
		t.Errorf("Expected status 'accepted', got %s", createResp["status"])
	}

	var statusResp map[string]any
	testutil.MustWaitFor(t, func() bool {
		resp, err = http.Get(baseURL + "/v1/jobs/" + jobID)
		if err != nil {
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			json.NewDecoder(resp.Body).Decode(&statusResp)
			return true
		}
		return false
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	if statusResp == nil {
		t.Fatal("Could not get job status")
	}

	if statusResp["id"] != jobID {
		t.Errorf("Expected job ID %s, got %v", jobID, statusResp["id"])
	}
}

func TestAPI_CreateAndCancelJob(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	jobID := fmt.Sprintf("e2e-cancel-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "sleep 300",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Create job failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}

	// Wait for job to be running before canceling
	testutil.MustWaitFor(t, func() bool {
		resp, err := http.Get(baseURL + "/v1/jobs/" + jobID)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var status map[string]any
		json.NewDecoder(resp.Body).Decode(&status)
		return status["status"] == "running"
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/v1/jobs/"+jobID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Cancel job failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("Expected status 204, got %d", resp.StatusCode)
	}

	resp, err = http.Get(baseURL + "/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("Get job failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404 after cancel, got %d", resp.StatusCode)
	}
}

func TestAPI_JobCompletion(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	jobID := fmt.Sprintf("e2e-complete-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "echo done",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Create job failed: %v", err)
	}
	resp.Body.Close()

	var status string
	testutil.MustWaitFor(t, func() bool {
		resp, err = http.Get(baseURL + "/v1/jobs/" + jobID)
		if err != nil {
			return false
		}
		defer resp.Body.Close()

		var statusResp map[string]any
		json.NewDecoder(resp.Body).Decode(&statusResp)

		if s, ok := statusResp["status"].(string); ok {
			status = s
			return status == "completed" || status == "failed"
		}
		return false
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	if status != "completed" {
		t.Errorf("Expected job to complete, got status: %s", status)
	}
}

func TestAPI_JobWithCallbacks(t *testing.T) {
	var eventCount atomic.Int64
	var mu sync.Mutex
	receivedEvents := make([]string, 0)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		json.NewDecoder(r.Body).Decode(&event)

		if eventType, ok := event["type"].(string); ok {
			mu.Lock()
			receivedEvents = append(receivedEvents, eventType)
			t.Logf("Received callback event: %s", eventType)
			mu.Unlock()
			eventCount.Add(1)
		}

		w.WriteHeader(http.StatusOK)
	})

	// Determine callback URL based on environment
	var callbackURL string
	var cleanup func()

	if callbackHost := os.Getenv("E2E_CALLBACK_HOST"); callbackHost != "" {
		// Use fixed port when running against external API (e.g., host.docker.internal)
		port := "19876"
		if p := os.Getenv("E2E_CALLBACK_PORT"); p != "" {
			port = p
		}

		server := &http.Server{Addr: ":" + port, Handler: handler}
		go func() {
			if err := server.ListenAndServe(); err != http.ErrServerClosed {
				t.Logf("Callback server error: %v", err)
			}
		}()

		callbackURL = fmt.Sprintf("http://%s:%s", callbackHost, port)
		cleanup = func() { server.Close() }
		t.Logf("Callback server listening on :%s, URL for jobs: %s", port, callbackURL)
	} else {
		// Use httptest server for local testing
		callbackServer := httptest.NewServer(handler)
		callbackURL = callbackServer.URL
		cleanup = callbackServer.Close
	}
	defer cleanup()

	baseURL, apiCleanup := getTestURL(t)
	defer apiCleanup()

	jobID := fmt.Sprintf("e2e-callback-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "echo 'callback test'",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
		"callback": map[string]any{
			"url": callbackURL,
		},
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Create job failed: %v", err)
	}
	resp.Body.Close()

	// Wait for at least 2 callback events (start, exit)
	testutil.MustWaitForCount(t, &eventCount, 2, testutil.WithTimeout(30*time.Second))

	mu.Lock()
	count := len(receivedEvents)
	events := make([]string, len(receivedEvents))
	copy(events, receivedEvents)
	mu.Unlock()

	t.Logf("Received %d callback events: %v", count, events)

	// Should receive at least start and exit events
	if count < 2 {
		t.Errorf("Expected at least 2 callback events (start, exit), got %d", count)
	}
}

func TestAPI_InvalidJobRequest(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	reqBody := map[string]any{
		"command": "echo hello",
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid request, got %d", resp.StatusCode)
	}
}

func TestAPI_ConcurrentJobs(t *testing.T) {
	baseURL, cleanup := getTestURL(t)
	defer cleanup()

	numJobs := 3
	var wg sync.WaitGroup
	errors := make(chan error, numJobs)

	for i := range numJobs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			jobID := fmt.Sprintf("e2e-concurrent-%d-%d", time.Now().UnixNano(), idx)

			reqBody := map[string]any{
				"id":             jobID,
				"image":          "alpine:latest",
				"command":        fmt.Sprintf("echo 'job %d' && sleep 2", idx),
				"cpu":            1,
				"memory":         128,
				"timeoutSeconds": 60,
			}
			body, _ := json.Marshal(reqBody)

			resp, err := http.Post(baseURL+"/v1/jobs", "application/json", bytes.NewReader(body))
			if err != nil {
				errors <- fmt.Errorf("job %d: create failed: %w", idx, err)
				return
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				errors <- fmt.Errorf("job %d: expected 202, got %d", idx, resp.StatusCode)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}
