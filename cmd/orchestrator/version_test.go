package main

import "testing"

func TestReleaseSidecarImage(t *testing.T) {
	previous := releaseVersion
	releaseVersion = "1.8.0"
	t.Cleanup(func() { releaseVersion = previous })

	if got, want := releaseSidecarImage("job-sidecar"), "ghcr.io/open-runtimes/orchestrator/job-sidecar:1.8.0"; got != want {
		t.Fatalf("releaseSidecarImage() = %q, want %q", got, want)
	}
	if got, want := releaseSidecarImage("workload-sidecar"), "ghcr.io/open-runtimes/orchestrator/workload-sidecar:1.8.0"; got != want {
		t.Fatalf("releaseSidecarImage() = %q, want %q", got, want)
	}
}

func TestReleaseSidecarImageFallsBackForLocalBuild(t *testing.T) {
	previous := releaseVersion
	releaseVersion = ""
	t.Cleanup(func() { releaseVersion = previous })

	if got, want := releaseSidecarImage("job-sidecar"), "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"; got != want {
		t.Fatalf("releaseSidecarImage() = %q, want %q", got, want)
	}
}

func TestConfiguredSidecarImageOverride(t *testing.T) {
	t.Setenv("JOB_SIDECAR_IMAGE", "registry.example/job-sidecar:custom")

	if got, want := configuredSidecarImage("JOB_SIDECAR_IMAGE", "job-sidecar"), "registry.example/job-sidecar:custom"; got != want {
		t.Fatalf("configuredSidecarImage() = %q, want %q", got, want)
	}
}
