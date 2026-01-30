package job

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/observability"
	"regexp"
	"strings"
)

// Validation limits
const (
	maxJobIDLength    = 128
	maxCPU            = 64    // cores
	maxMemory         = 65536 // MB (64GB)
	maxTimeoutSecs    = 86400 // 24 hours
	maxMetaKeyLen     = 64
	maxMetaValueLen   = 256
	maxMetaEntries    = 32
	maxArtifacts      = 64
	maxCallbackEvents = 16
)

// jobIDPattern allows alphanumeric, hyphens, and underscores
var jobIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Service manages job lifecycle using an orchestrator.
//
// The Service is stateless - all job state lives in the orchestrator.
// This enables:
//   - Service restarts without affecting running jobs
//   - Horizontal scaling with multiple service instances
//   - Cross-instance job queries and cancellation
type Service struct {
	orchestrator Orchestrator
	metrics      *observability.Metrics
}

// NewService creates a new job service.
func NewService(orchestrator Orchestrator, metrics *observability.Metrics) *Service {
	return &Service{
		orchestrator: orchestrator,
		metrics:      metrics,
	}
}

// Create validates and starts a new job.
// Note: This method applies defaults to the request before validation.
func (s *Service) Create(ctx context.Context, req *Request) (*Response, error) {
	applyDefaults(req)
	if err := s.validate(req); err != nil {
		return nil, err
	}

	logger := slog.With("jobId", req.ID, "image", req.Image)

	if err := s.orchestrator.Run(ctx, req); err != nil {
		logger.Error("Job failed to start", "error", err)
		return nil, err
	}

	// Record metrics after successful creation
	if s.metrics != nil {
		s.metrics.RecordJobCreated(ctx, req.Image)
	}

	logger.Info("Job created")

	return &Response{
		ID:     req.ID,
		Status: StateAccepted,
	}, nil
}

// Get returns the status of a job.
func (s *Service) Get(ctx context.Context, jobID string) (*Status, error) {
	return s.orchestrator.Status(ctx, jobID)
}

// Cancel stops a running job.
func (s *Service) Cancel(ctx context.Context, jobID string) error {
	logger := slog.With("jobId", jobID)
	if err := s.orchestrator.Stop(ctx, jobID); err != nil {
		logger.Error("Job cancellation failed", "error", err)
		return err
	}
	logger.Info("Job cancelled")
	return nil
}

// List returns all jobs and their statuses.
func (s *Service) List(ctx context.Context) (*ListResponse, error) {
	statuses, err := s.orchestrator.List(ctx)
	if err != nil {
		return nil, err
	}
	return &ListResponse{Jobs: statuses}, nil
}

// applyDefaults sets default values for unspecified request fields.
func applyDefaults(req *Request) {
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 1800
	}
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Memory <= 0 {
		req.Memory = 512
	}
	if req.Workspace == "" {
		req.Workspace = "/workspace"
	}
}

// validate validates a job request. Does not modify the request.
func (s *Service) validate(req *Request) error {
	if req.ID == "" {
		return apperrors.Validation("id", "job ID is required")
	}
	if len(req.ID) > maxJobIDLength {
		return apperrors.Validation("id", fmt.Sprintf("job ID exceeds maximum length of %d", maxJobIDLength))
	}
	if !jobIDPattern.MatchString(req.ID) {
		return apperrors.Validation("id", "job ID must be alphanumeric (hyphens and underscores allowed, cannot start with hyphen/underscore)")
	}

	if req.Image == "" {
		return apperrors.Validation("image", "image is required")
	}

	if req.TimeoutSeconds > maxTimeoutSecs {
		return apperrors.Validation("timeoutSeconds", fmt.Sprintf("timeout exceeds maximum of %d seconds", maxTimeoutSecs))
	}

	if req.CPU > maxCPU {
		return apperrors.Validation("cpu", fmt.Sprintf("CPU exceeds maximum of %d cores", maxCPU))
	}

	if req.Memory > maxMemory {
		return apperrors.Validation("memory", fmt.Sprintf("memory exceeds maximum of %d MB", maxMemory))
	}

	// Validate metadata
	if len(req.Meta) > maxMetaEntries {
		return apperrors.Validation("meta", fmt.Sprintf("metadata exceeds maximum of %d entries", maxMetaEntries))
	}
	for k, v := range req.Meta {
		if len(k) > maxMetaKeyLen {
			return apperrors.Validation("meta", fmt.Sprintf("metadata key exceeds maximum length of %d", maxMetaKeyLen))
		}
		if len(v) > maxMetaValueLen {
			return apperrors.Validation("meta", fmt.Sprintf("metadata value exceeds maximum length of %d", maxMetaValueLen))
		}
	}

	// Validate artifacts
	if len(req.Artifacts) > maxArtifacts {
		return apperrors.Validation("artifacts", fmt.Sprintf("artifacts exceed maximum of %d", maxArtifacts))
	}
	for i, a := range req.Artifacts {
		if err := artifact.Validate(i, a); err != nil {
			return err
		}
	}

	// Validate callback
	if req.Callback != nil {
		if req.Callback.URL != "" {
			if err := validateURL(req.Callback.URL); err != nil {
				return apperrors.Validation("callback.url", fmt.Sprintf("invalid callback URL: %v", err))
			}
		}
		if len(req.Callback.Events) > maxCallbackEvents {
			return apperrors.Validation("callback.events", fmt.Sprintf("callback events exceed maximum of %d", maxCallbackEvents))
		}
	}

	return nil
}

func validateURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("URL must have a host")
	}
	return nil
}
