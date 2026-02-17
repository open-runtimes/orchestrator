package docker

import (
	"context"
	"encoding/json"
	"orchestrator/internal/job"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// watchConfig holds everything needed to watch a job's lifecycle.
// Built from either a job.Request (normal path) or containerState (resume path).
type watchConfig struct {
	jobID string
	image string
	dest  *callbackDest
}

// containerState is the Docker state needed to reconstruct a watchConfig and
// jobWatchState. Collected once via inspectContainers, then mapped purely in memory.
type containerState struct {
	jobID          string
	workerState    string            // "created", "running", "exited", "dead", or "" if no worker
	workerExitCode int               // exit code of the worker container
	workerImage    string            // e.g. "alpine:latest"
	workerLabels   map[string]string // worker container labels (callback config, meta)
	startedAt      time.Time         // when the worker started (zero if never started)
}

// inspectContainers reads the Docker state needed for mapping.
func inspectContainers(ctx context.Context, cli *client.Client, jobID, workerID, sidecarID string) containerState {
	cs := containerState{jobID: jobID}

	if workerID != "" {
		if info, err := cli.ContainerInspect(ctx, workerID); err == nil {
			cs.workerState = info.State.Status
			cs.workerExitCode = info.State.ExitCode
			cs.workerImage = info.Config.Image
			cs.workerLabels = info.Config.Labels
			if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil {
				cs.startedAt = t
			}
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

// watchConfigFromState maps inspected Docker container state to a watchConfig
// and initial jobWatchState. This is a pure function — no API calls.
func watchConfigFromState(cs containerState) (*watchConfig, *jobWatchState) {
	cfg := &watchConfig{
		jobID: cs.jobID,
		image: cs.workerImage,
		dest:  callbackDestFromLabels(cs.jobID, cs.workerLabels),
	}

	state := &jobWatchState{}
	switch cs.workerState {
	case "running", "exited", "dead":
		state.workerStarted = true
		state.startTime = cs.startedAt
	}

	return cfg, state
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
