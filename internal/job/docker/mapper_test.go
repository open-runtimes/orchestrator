package docker

import (
	"orchestrator/internal/job"
	vol "orchestrator/internal/volume"
	"reflect"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestClampCPU(t *testing.T) {
	t.Parallel()
	cores := float64(runtime.NumCPU())

	// A request within the host's core count passes through unchanged.
	if got := clampCPU(0.5); got != 0.5 {
		t.Errorf("clampCPU(0.5) = %v, want 0.5", got)
	}
	if got := clampCPU(cores); got != cores {
		t.Errorf("clampCPU(%v) = %v, want %v", cores, got, cores)
	}
	// A request above the host's cores is clamped down so Docker won't reject it.
	if got := clampCPU(cores + 4); got != cores {
		t.Errorf("clampCPU(%v) = %v, want %v", cores+4, got, cores)
	}
}

func TestVolumeMounts(t *testing.T) {
	t.Parallel()
	mounts := volumeMounts([]vol.Volume{
		{Source: "data", Path: "/data", ReadOnly: true},
		{Source: "cache", Path: "/cache", SubPath: "sub"},
	})
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	if mounts[0].Type != mount.TypeVolume || mounts[0].Source != "data" || mounts[0].Target != "/data" || !mounts[0].ReadOnly {
		t.Errorf("mount[0] = %+v", mounts[0])
	}
	if mounts[0].VolumeOptions != nil {
		t.Error("mount[0] should have no VolumeOptions without a subPath")
	}
	if mounts[1].VolumeOptions == nil || mounts[1].VolumeOptions.Subpath != "sub" {
		t.Errorf("mount[1] subPath = %+v, want sub", mounts[1].VolumeOptions)
	}
}

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
	if cfg.dest.URL != "https://example.com/cb" {
		t.Errorf("dest.URL: want https://example.com/cb, got %s", cfg.dest.URL)
	}
	if cfg.dest.Key != "secret" {
		t.Errorf("dest.Key: want secret, got %s", cfg.dest.Key)
	}
	if !reflect.DeepEqual(cfg.dest.Events, req.Callback.Events) {
		t.Errorf("dest.Events: want %v, got %v", req.Callback.Events, cfg.dest.Events)
	}
	if !reflect.DeepEqual(cfg.dest.Meta, req.Meta) {
		t.Errorf("dest.Meta: want %v, got %v", req.Meta, cfg.dest.Meta)
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
	if cfg.dest.URL != "https://example.com/cb" {
		t.Errorf("dest.URL: want https://example.com/cb, got %s", cfg.dest.URL)
	}
	if cfg.dest.Key != "secret" {
		t.Errorf("dest.Key: want secret, got %s", cfg.dest.Key)
	}
}

// --- callbackDestFromLabels ---

func TestCallbackDestFromLabels_NoURL(t *testing.T) {
	t.Parallel()
	for _, labels := range []map[string]string{nil, {}, {"job.callback.key": "k"}} {
		if dest := callbackDestFromLabels(labels); dest != nil {
			t.Errorf("want nil when no callback URL, got %+v", dest)
		}
	}
}

func TestCallbackDestFromLabels_URLOnly(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"job.callback.url": "https://example.com/cb"}

	dest := callbackDestFromLabels(labels)

	if dest == nil {
		t.Fatal("want non-nil dest")
	}
	if dest.URL != "https://example.com/cb" {
		t.Errorf("URL: want https://example.com/cb, got %s", dest.URL)
	}
	if dest.Key != "" {
		t.Errorf("Key: want empty, got %s", dest.Key)
	}
	if dest.Events != nil {
		t.Errorf("Events: want nil, got %v", dest.Events)
	}
	if dest.Meta != nil {
		t.Errorf("Meta: want nil, got %v", dest.Meta)
	}
}

func TestCallbackDestFromLabels_WithKey(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.callback.key": "my-secret",
	}

	dest := callbackDestFromLabels(labels)

	if dest.Key != "my-secret" {
		t.Errorf("Key: want my-secret, got %s", dest.Key)
	}
}

func TestCallbackDestFromLabels_WithEvents(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url":    "https://example.com/cb",
		"job.callback.events": "orchestrator.job.start,orchestrator.job.exit",
	}

	dest := callbackDestFromLabels(labels)

	want := []string{"orchestrator.job.start", "orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.Events, want) {
		t.Errorf("Events: want %v, got %v", want, dest.Events)
	}
}

func TestCallbackDestFromLabels_SingleEvent(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url":    "https://example.com/cb",
		"job.callback.events": "orchestrator.job.exit",
	}

	dest := callbackDestFromLabels(labels)

	want := []string{"orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.Events, want) {
		t.Errorf("Events: want %v, got %v", want, dest.Events)
	}
}

func TestCallbackDestFromLabels_WithMeta(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.meta":         `{"env":"prod","region":"us-east-1"}`,
	}

	dest := callbackDestFromLabels(labels)

	want := map[string]string{"env": "prod", "region": "us-east-1"}
	if !reflect.DeepEqual(dest.Meta, want) {
		t.Errorf("Meta: want %v, got %v", want, dest.Meta)
	}
}

func TestCallbackDestFromLabels_InvalidMetaJSON(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"job.callback.url": "https://example.com/cb",
		"job.meta":         `not-valid-json`,
	}

	dest := callbackDestFromLabels(labels)

	// Invalid JSON should be silently ignored; dest is still returned
	if dest == nil {
		t.Fatal("want non-nil dest even with invalid meta JSON")
	}
	if dest.Meta != nil {
		t.Errorf("Meta: want nil on invalid JSON, got %v", dest.Meta)
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

	dest := callbackDestFromLabels(labels)

	if dest == nil {
		t.Fatal("want non-nil dest")
	}
	if dest.URL != "https://example.com/cb" {
		t.Error("URL mismatch")
	}
	if dest.Key != "secret" {
		t.Error("Key mismatch")
	}
	wantEvents := []string{"orchestrator.job.start", "orchestrator.job.log", "orchestrator.job.exit"}
	if !reflect.DeepEqual(dest.Events, wantEvents) {
		t.Errorf("Events: want %v, got %v", wantEvents, dest.Events)
	}
	wantMeta := map[string]string{"team": "platform"}
	if !reflect.DeepEqual(dest.Meta, wantMeta) {
		t.Errorf("Meta: want %v, got %v", wantMeta, dest.Meta)
	}
}
