package docker

import (
	"orchestrator/internal/job"
	"reflect"
	"testing"
)

// --- watchConfigFromRequest ---

func TestWatchConfigFromRequest_NoCallback(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-1", Image: "alpine:latest"}
	h := dockerHandle{sidecarContainerID: "sc-1", jobContainerID: "jc-1"}

	cfg := watchConfigFromRequest(req, h)

	if cfg.jobID != "job-1" {
		t.Errorf("jobID: want job-1, got %s", cfg.jobID)
	}
	if cfg.image != "alpine:latest" {
		t.Errorf("image: want alpine:latest, got %s", cfg.image)
	}
	if cfg.sidecarID != "sc-1" {
		t.Errorf("sidecarID: want sc-1, got %s", cfg.sidecarID)
	}
	if cfg.workerID != "jc-1" {
		t.Errorf("workerID: want jc-1, got %s", cfg.workerID)
	}
	if cfg.dest != nil {
		t.Error("dest: want nil when no callback configured")
	}
}

func TestWatchConfigFromRequest_NilCallback(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-1", Image: "alpine:latest", Callback: nil}

	cfg := watchConfigFromRequest(req, dockerHandle{})

	if cfg.dest != nil {
		t.Error("dest: want nil when Callback is nil")
	}
}

func TestWatchConfigFromRequest_EmptyCallbackURL(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:       "job-1",
		Image:    "alpine:latest",
		Callback: &job.Callback{URL: ""},
	}

	cfg := watchConfigFromRequest(req, dockerHandle{})

	if cfg.dest != nil {
		t.Error("dest: want nil when Callback.URL is empty")
	}
}

func TestWatchConfigFromRequest_FullCallback(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:    "job-1",
		Image: "alpine:latest",
		Meta:  map[string]string{"env": "prod"},
		Callback: &job.Callback{
			URL:    "https://example.com/cb",
			Key:    "secret",
			Events: []string{"orchestrator.job.start", "orchestrator.job.exit"},
		},
	}

	cfg := watchConfigFromRequest(req, dockerHandle{})

	if cfg.dest == nil {
		t.Fatal("dest: want non-nil")
	}
	if cfg.dest.jobID != "job-1" {
		t.Errorf("dest.jobID: want job-1, got %s", cfg.dest.jobID)
	}
	if cfg.dest.url != "https://example.com/cb" {
		t.Errorf("dest.url: want https://example.com/cb, got %s", cfg.dest.url)
	}
	if cfg.dest.key != "secret" {
		t.Errorf("dest.key: want secret, got %s", cfg.dest.key)
	}
	if !reflect.DeepEqual(cfg.dest.events, req.Callback.Events) {
		t.Errorf("dest.events: want %v, got %v", req.Callback.Events, cfg.dest.events)
	}
	if !reflect.DeepEqual(cfg.dest.meta, req.Meta) {
		t.Errorf("dest.meta: want %v, got %v", req.Meta, cfg.dest.meta)
	}
}

// --- watchConfigFromState ---

func TestWatchConfigFromState_NoCallbackLabels(t *testing.T) {
	t.Parallel()
	cs := containerState{
		jobID:       "job-1",
		workerImage: "alpine:latest",
	}
	h := dockerHandle{sidecarContainerID: "sc-1", jobContainerID: "jc-1"}

	cfg := watchConfigFromState(cs, h)

	if cfg.jobID != "job-1" {
		t.Errorf("jobID: want job-1, got %s", cfg.jobID)
	}
	if cfg.image != "alpine:latest" {
		t.Errorf("image: want alpine:latest, got %s", cfg.image)
	}
	if cfg.sidecarID != "sc-1" {
		t.Errorf("sidecarID: want sc-1, got %s", cfg.sidecarID)
	}
	if cfg.workerID != "jc-1" {
		t.Errorf("workerID: want jc-1, got %s", cfg.workerID)
	}
	if cfg.dest != nil {
		t.Error("dest: want nil when no callback labels")
	}
}

func TestWatchConfigFromState_WithCallbackLabels(t *testing.T) {
	t.Parallel()
	cs := containerState{
		jobID:       "job-1",
		workerImage: "alpine:latest",
		workerLabels: map[string]string{
			"job.callback.url": "https://example.com/cb",
			"job.callback.key": "secret",
		},
	}

	cfg := watchConfigFromState(cs, dockerHandle{})

	if cfg.dest == nil {
		t.Fatal("dest: want non-nil")
	}
	if cfg.dest.url != "https://example.com/cb" {
		t.Errorf("dest.url: want https://example.com/cb, got %s", cfg.dest.url)
	}
	if cfg.dest.key != "secret" {
		t.Errorf("dest.key: want secret, got %s", cfg.dest.key)
	}
}

// --- callbackDestFromLabels ---

func TestCallbackDestFromLabels_NoURL(t *testing.T) {
	t.Parallel()
	for _, labels := range []map[string]string{nil, {}, {"job.callback.key": "k"}} {
		if dest := callbackDestFromLabels("job-1", labels); dest != nil {
			t.Errorf("want nil when no callback URL, got %+v", dest)
		}
	}
}

func TestCallbackDestFromLabels_URLOnly(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"job.callback.url": "https://example.com/cb"}

	dest := callbackDestFromLabels("job-1", labels)

	if dest == nil {
		t.Fatal("want non-nil dest")
	}
	if dest.jobID != "job-1" {
		t.Errorf("jobID: want job-1, got %s", dest.jobID)
	}
	if dest.url != "https://example.com/cb" {
		t.Errorf("url: want https://example.com/cb, got %s", dest.url)
	}
	if dest.key != "" {
		t.Errorf("key: want empty, got %s", dest.key)
	}
	if dest.events != nil {
		t.Errorf("events: want nil, got %v", dest.events)
	}
	if dest.meta != nil {
		t.Errorf("meta: want nil, got %v", dest.meta)
	}
}

func TestCallbackDestFromLabels_WithKey(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.callback.key": "my-secret",
	}

	dest := callbackDestFromLabels("job-1", labels)

	if dest.key != "my-secret" {
		t.Errorf("key: want my-secret, got %s", dest.key)
	}
}

func TestCallbackDestFromLabels_WithEvents(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url":    "https://example.com/cb",
		"job.callback.events": "orchestrator.job.start,orchestrator.job.exit",
	}

	dest := callbackDestFromLabels("job-1", labels)

	want := []string{"orchestrator.job.start", "orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.events, want) {
		t.Errorf("events: want %v, got %v", want, dest.events)
	}
}

func TestCallbackDestFromLabels_SingleEvent(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url":    "https://example.com/cb",
		"job.callback.events": "orchestrator.job.exit",
	}

	dest := callbackDestFromLabels("job-1", labels)

	want := []string{"orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.events, want) {
		t.Errorf("events: want %v, got %v", want, dest.events)
	}
}

func TestCallbackDestFromLabels_WithMeta(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.meta":         `{"env":"prod","region":"us-east-1"}`,
	}

	dest := callbackDestFromLabels("job-1", labels)

	want := map[string]string{"env": "prod", "region": "us-east-1"}
	if !reflect.DeepEqual(dest.meta, want) {
		t.Errorf("meta: want %v, got %v", want, dest.meta)
	}
}

func TestCallbackDestFromLabels_InvalidMetaJSON(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.meta":         `not-valid-json`,
	}

	dest := callbackDestFromLabels("job-1", labels)

	// Invalid JSON should be silently ignored; dest is still returned
	if dest == nil {
		t.Fatal("want non-nil dest even with invalid meta JSON")
	}
	if dest.meta != nil {
		t.Errorf("meta: want nil on invalid JSON, got %v", dest.meta)
	}
}

func TestCallbackDestFromLabels_AllFields(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url":    "https://example.com/cb",
		"job.callback.key":    "secret",
		"job.callback.events": "orchestrator.job.start,orchestrator.job.log,orchestrator.job.exit",
		"job.meta":            `{"team":"platform"}`,
	}

	dest := callbackDestFromLabels("job-42", labels)

	if dest == nil {
		t.Fatal("want non-nil dest")
	}
	if dest.jobID != "job-42" {
		t.Errorf("jobID: want job-42, got %s", dest.jobID)
	}
	if dest.url != "https://example.com/cb" {
		t.Errorf("url mismatch")
	}
	if dest.key != "secret" {
		t.Errorf("key mismatch")
	}
	wantEvents := []string{"orchestrator.job.start", "orchestrator.job.log", "orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.events, wantEvents) {
		t.Errorf("events: want %v, got %v", wantEvents, dest.events)
	}
	wantMeta := map[string]string{"team": "platform"}
	if !reflect.DeepEqual(dest.meta, wantMeta) {
		t.Errorf("meta: want %v, got %v", wantMeta, dest.meta)
	}
}
