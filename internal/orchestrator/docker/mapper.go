package docker

import (
	"context"
	"encoding/json"
	"orchestrator/internal/job"
	"strings"

	"github.com/docker/docker/client"
)

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
	jobID          string
	workerExitCode int               // exit code of the worker container
	workerImage    string            // e.g. "alpine:latest"
	workerLabels   map[string]string // worker container labels (callback config, meta)
}

// inspectContainers reads the Docker state needed for resume mapping.
func inspectContainers(ctx context.Context, cli *client.Client, jobID, workerID string) containerState {
	cs := containerState{jobID: jobID}

	if workerID != "" {
		if info, err := cli.ContainerInspect(ctx, workerID); err == nil {
			cs.workerExitCode = info.State.ExitCode
			cs.workerImage = info.Config.Image
			cs.workerLabels = info.Config.Labels
		}
	}

	return cs
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
		_ = json.Unmarshal([]byte(raw), &meta)
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
