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
	"orchestrator/internal/artifact"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/orchestrator/docker"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/job"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

type testAPI struct {
	baseURL string
	cleanup func()
	jobIDs  sync.Map
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()

	baseURL, cleanup := getTestURL(t)
	testClient := &testAPI{
		baseURL: baseURL,
		cleanup: cleanup,
	}
	t.Cleanup(testClient.cleanupAll)

	return testClient
}

func (a *testAPI) createJob(t *testing.T, body map[string]any) *http.Response {
	t.Helper()

	resp, err := a.createJobRequest(body)
	if err != nil {
		t.Fatalf("Create job failed: %v", err)
	}

	return resp
}

func (a *testAPI) createJobRequest(body map[string]any) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	resp, err := http.Post(a.baseURL+"/v1/jobs", "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	if id, ok := body["id"].(string); ok && resp.StatusCode == http.StatusAccepted {
		a.jobIDs.Store(id, struct{}{})
	}

	return resp, nil
}

func (a *testAPI) cleanupAll() {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	a.jobIDs.Range(func(key, _ any) bool {
		jobID, ok := key.(string)
		if !ok {
			return true
		}

		req, err := http.NewRequest(http.MethodDelete, a.baseURL+"/v1/jobs/"+jobID, nil)
		if err == nil {
			resp, err := httpClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}

		a.waitForJobCleanup(jobID)
		a.cleanupDockerJob(jobID)

		return true
	})

	a.cleanup()
}

func (a *testAPI) waitForJobCleanup(jobID string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(a.baseURL + "/v1/jobs/" + jobID)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *testAPI) cleanupDockerJob(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()

	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "managed-by=jobs-service"),
			filters.Arg("label", "job.id="+jobID),
		),
	})
	if err == nil {
		for _, c := range containers {
			_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	_ = cli.VolumeRemove(ctx, "job-"+jobID+"-workspace", true)
}

// getTestURL returns the base URL for e2e tests.
// If E2E_API_URL is set, tests run against that instance.
// Otherwise, a test server is created.
func getTestURL(t *testing.T) (string, func()) {
	t.Helper()
	if url := os.Getenv("E2E_API_URL"); url != "" {
		t.Logf("Using external API: %s", url)
		return url, func() {}
	}

	server, _, cleanup := createTestServer(t)
	return server.URL, cleanup
}

func createTestServer(t *testing.T) (*httptest.Server, *job.Service, func()) {
	t.Helper()
	eventDispatcher := dispatcher.NewMemory(dispatcher.Config{
		BufferSize: 100,
		Workers:    2,
	}, nil)

	emitter := job.NewCallbackEmitter()
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.CallbackURL == "" {
			return
		}
		_ = eventDispatcher.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		})
	})

	orchestrator, err := job.NewOrchestrator(emitter, docker.NewOrchestrator(t.Context(), docker.Config{
		SidecarImage: "ko.local/job-sidecar:latest",
	}))
	if err != nil {
		t.Fatalf("Failed to create Docker orchestrator: %v", err)
	}

	svc := job.NewService(orchestrator, nil, artifact.DefaultRegistry(), "")
	healthChecker := health.NewChecker(orchestrator)

	routerCfg := api.RouterConfig{
		JobService:    svc,
		HealthChecker: healthChecker,
	}
	if ae, ok := orchestrator.(api.ArtifactEmitter); ok {
		routerCfg.ArtifactEmitter = ae
	}
	router := api.NewRouter(routerCfg)

	server := httptest.NewServer(router)

	cleanup := func() {
		orchestrator.Close()
		// Drain dispatcher before closing server so pending callbacks can be delivered
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	testClient := newTestAPI(t)

	jobID := fmt.Sprintf("e2e-test-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "echo 'hello' && sleep 5",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	resp := testClient.createJob(t, reqBody)
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
		r, e := http.Get(testClient.baseURL + "/v1/jobs/" + jobID)
		if e != nil {
			return false
		}
		defer r.Body.Close()

		if r.StatusCode == http.StatusOK {
			json.NewDecoder(r.Body).Decode(&statusResp)
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
	testClient := newTestAPI(t)

	jobID := fmt.Sprintf("e2e-cancel-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "sleep 300",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	resp := testClient.createJob(t, reqBody)
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}

	// Wait for job to be running before canceling
	testutil.MustWaitFor(t, func() bool {
		resp, err := http.Get(testClient.baseURL + "/v1/jobs/" + jobID)
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

	req, _ := http.NewRequest(http.MethodDelete, testClient.baseURL+"/v1/jobs/"+jobID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Cancel job failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("Expected status 204, got %d", resp.StatusCode)
	}

	resp, err = http.Get(testClient.baseURL + "/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("Get job failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404 after cancel, got %d", resp.StatusCode)
	}
}

func TestAPI_JobCompletion(t *testing.T) {
	testClient := newTestAPI(t)

	jobID := fmt.Sprintf("e2e-complete-%d", time.Now().UnixNano())

	reqBody := map[string]any{
		"id":             jobID,
		"image":          "alpine:latest",
		"command":        "echo done",
		"cpu":            1,
		"memory":         128,
		"timeoutSeconds": 60,
	}
	resp := testClient.createJob(t, reqBody)
	resp.Body.Close()

	var status string
	testutil.MustWaitFor(t, func() bool {
		r, e := http.Get(testClient.baseURL + "/v1/jobs/" + jobID)
		if e != nil {
			return false
		}
		defer r.Body.Close()

		var statusResp map[string]any
		json.NewDecoder(r.Body).Decode(&statusResp)

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

		callbackURL = "http://" + net.JoinHostPort(callbackHost, port)
		cleanup = func() { server.Close() }
		t.Logf("Callback server listening on :%s, URL for jobs: %s", port, callbackURL)
	} else {
		// Use httptest server for local testing
		callbackServer := httptest.NewServer(handler)
		callbackURL = callbackServer.URL
		cleanup = callbackServer.Close
	}
	defer cleanup()

	testClient := newTestAPI(t)

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
	resp := testClient.createJob(t, reqBody)
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
	testClient := newTestAPI(t)

	numJobs := 3
	var wg sync.WaitGroup
	errors := make(chan error, numJobs)

	for i := range numJobs {
		wg.Go(func() {
			jobID := fmt.Sprintf("e2e-concurrent-%d-%d", time.Now().UnixNano(), i)

			reqBody := map[string]any{
				"id":             jobID,
				"image":          "alpine:latest",
				"command":        fmt.Sprintf("echo 'job %d' && sleep 2", i),
				"cpu":            1,
				"memory":         128,
				"timeoutSeconds": 60,
			}
			resp, err := testClient.createJobRequest(reqBody)
			if err != nil {
				errors <- fmt.Errorf("job %d: create job failed: %w", i, err)
				return
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				errors <- fmt.Errorf("job %d: expected 202, got %d", i, resp.StatusCode)
				return
			}
		})
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}
