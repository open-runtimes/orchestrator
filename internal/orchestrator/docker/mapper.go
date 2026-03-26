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
	jobID string
	image string
	dest  *callbackDest
}

// containerState is the Docker state needed to reconstruct a watchConfig on resume.
// Collected once via inspectContainers, then mapped purely in memory.
type containerState struct {
	jobID        string
	workerExitCode int            // exit code of the worker container
	workerImage  string           // e.g. "alpine:latest"
	workerLabels map[string]string // worker container labels (callback config, meta)
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

// watchConfigFromRequest maps a job.Request to a watchConfig.
func watchConfigFromRequest(req *job.Request) *watchConfig {
	cfg := &watchConfig{
		jobID: req.ID,
		image: req.Image,
	}
	if req.Callback != nil && req.Callback.URL != "" {
		cfg.dest = &callbackDest{
			jobID:  req.ID,
			meta:   req.Meta,
			url:    req.Callback.URL,
			key:    req.Callback.Key,
			events: req.Callback.Events,
		}
	}
	return cfg
}

// watchConfigFromState maps inspected Docker container state to a watchConfig.
// This is a pure function — no API calls.
func watchConfigFromState(cs containerState) *watchConfig {
	return &watchConfig{
		jobID: cs.jobID,
		image: cs.workerImage,
		dest:  callbackDestFromLabels(cs.jobID, cs.workerLabels),
	}
}

// callbackDestFromLabels parses callback destination from worker container labels.
// The original (non-proxy-rewritten) callback URL is stored here at creation time.
func callbackDestFromLabels(jobID string, labels map[string]string) *callbackDest {
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

	return &callbackDest{
		jobID:  jobID,
		meta:   meta,
		url:    callbackURL,
		key:    labels["job.callback.key"],
		events: events,
	}
}
