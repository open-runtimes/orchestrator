package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/job"
	"runtime"
	"strings"

	"github.com/docker/docker/client"
)

// clampCPU limits a CPU request to the host's core count. Docker rejects a
// NanoCPUs value above the number of cores on the daemon, so a request larger
// than the build host (e.g. 8 CPUs on a 4-core host) would otherwise fail
// container creation outright.
func clampCPU(cpu float64) float64 {
	if maxCPU := float64(runtime.NumCPU()); cpu > maxCPU {
		return maxCPU
	}
	return cpu
}

// watchConfig holds everything needed to watch a job's lifecycle.
// Built from either a job.Request (normal path) or containerState (resume path).
type watchConfig struct {
	jobID     string
	image     string
	sidecarID string
	workerID  string
	dest      *job.CallbackDest
}

// containerState is the Docker state needed to reconstruct a watchConfig on resume.
// Collected once via inspectContainers, then mapped purely in memory.
type containerState struct {
	jobID           string
	workerExitCode  int               // exit code of the worker container
	workerOOMKilled bool              // worker was killed by the kernel OOM killer
	workerImage     string            // e.g. "alpine:latest"
	workerLabels    map[string]string // worker container labels (callback config, meta)
}

// inspectContainers reads the Docker state needed for resume mapping.
func inspectContainers(ctx context.Context, cli *client.Client, jobID, workerID string) (containerState, error) {
	cs := containerState{jobID: jobID}
	if workerID == "" {
		return cs, nil
	}
	info, err := cli.ContainerInspect(ctx, workerID)
	if err != nil {
		return cs, err
	}
	cs.workerExitCode = info.State.ExitCode
	cs.workerOOMKilled = info.State.OOMKilled
	cs.workerImage = info.Config.Image
	cs.workerLabels = info.Config.Labels
	return cs, nil
}

// watchConfigFromRequest maps a job.Request and dockerHandle to a watchConfig.
func watchConfigFromRequest(req *job.Request, h dockerHandle) *watchConfig {
	cfg := &watchConfig{
		jobID:     req.ID,
		image:     req.Image,
		sidecarID: h.sidecarContainerID,
		workerID:  h.jobContainerID,
	}
	if req.Callback != nil && req.Callback.URL != "" {
		cfg.dest = &job.CallbackDest{
			Meta:   req.Meta,
			URL:    req.Callback.URL,
			Key:    req.Callback.Key,
			Events: req.Callback.Events,
		}
	}
	return cfg
}

// watchConfigFromState maps inspected Docker container state and a dockerHandle to a watchConfig.
// This is a pure function — no API calls.
func watchConfigFromState(cs containerState, h dockerHandle) *watchConfig {
	return &watchConfig{
		jobID:     cs.jobID,
		image:     cs.workerImage,
		sidecarID: h.sidecarContainerID,
		workerID:  h.jobContainerID,
		dest:      callbackDestFromLabels(cs.workerLabels),
	}
}

// callbackDestFromLabels parses callback destination from worker container labels.
// Callback config is stored as labels at creation time so it survives service restarts.
func callbackDestFromLabels(labels map[string]string) *job.CallbackDest {
	callbackURL := labels["job.callback.url"]
	if callbackURL == "" {
		return nil
	}

	var meta map[string]string
	if raw := labels["job.meta"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			slog.Warn("Ignoring undecodable job.meta label", "error", err)
		}
	}

	var events []string
	if raw := labels["job.callback.events"]; raw != "" {
		events = strings.Split(raw, ",")
	}

	return &job.CallbackDest{
		Meta:   meta,
		URL:    callbackURL,
		Key:    labels["job.callback.key"],
		Events: events,
	}
}

// sidecarEnv is the job sidecar's environment. It carries what the sidecar
// needs to run the artifacts and report on them, and nothing about where those
// reports end up: the callback URL, its signing key and the job's meta stay
// with the service, which looks them up when a report arrives.
func sidecarEnv(req *job.Request, artifactEndpoint string) ([]string, error) {
	env := []string{
		"JOB_ID=" + req.ID,
		fmt.Sprintf("TIMEOUT_SECONDS=%d", req.TimeoutSeconds),
		config.EnvSharedVolume + "=" + req.Workspace,
	}

	// MarshalArtifacts injects each artifact's "type" field, which the sidecar
	// needs to unmarshal them back into concrete types.
	artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal artifacts: %w", err)
	}
	env = append(env, "ARTIFACTS_JSON="+string(artifactsJSON))

	if artifactEndpoint != "" {
		env = append(env, "ARTIFACT_ENDPOINT="+artifactEndpoint)
	}
	if req.ArtifactToken != "" {
		env = append(env, "ARTIFACT_TOKEN="+req.ArtifactToken)
	}

	for _, kv := range config.LoadS3Credentials().ToEnv() {
		env = append(env, kv[0]+"="+kv[1])
	}
	return env, nil
}
