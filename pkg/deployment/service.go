package deployment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"regexp"
	"strings"
)

// Validation limits (shared scale with pkg/job where the concept matches).
const (
	maxIDLength     = 63  // RFC-1123 label: becomes part of object names
	maxHostLength   = 253 // RFC-1123 subdomain
	maxCPU          = 64
	maxMemory       = 65536
	maxTimeoutSecs  = 3600
	maxMetaEntries  = 32
	maxMetaKeyLen   = 64
	maxMetaValueLen = 256
	maxArtifacts    = 64
	maxReplicas     = 32
)

// Defaults applied by Apply.
const (
	DefaultTimeoutSeconds              = 300
	DefaultResponseStartTimeoutSeconds = 300
	DefaultProgressDeadlineSeconds     = 600
)

// idPattern is an RFC-1123 label: lowercase alphanumeric with interior hyphens.
var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// hostPattern is an RFC-1123 subdomain: dot-separated labels.
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// URLBuilder derives the deployment's public URL from its host. Supplied by
// the caller because the data-plane address (activator port, base domain) is
// deployment-service config, not backend state.
type URLBuilder func(host string) string

// Service manages deployment lifecycle using an orchestrator. Stateless —
// all deployment state lives in the backend.
type Service struct {
	orchestrator Orchestrator
	artifacts    *artifact.Registry
	domain       string // base domain for auto-assigned hosts: {id}.{domain}
	urlFor       URLBuilder
}

// NewService creates a deployment service. domain is the base for
// auto-assigned hosts; urlFor renders a host into the public URL.
func NewService(orchestrator Orchestrator, artifacts *artifact.Registry, domain string, urlFor URLBuilder) *Service {
	return &Service{
		orchestrator: orchestrator,
		artifacts:    artifacts,
		domain:       domain,
		urlFor:       urlFor,
	}
}

// Apply validates the spec (after applying defaults) and creates-or-updates
// the deployment.
func (s *Service) Apply(ctx context.Context, req *Request) (*StatusResponse, error) {
	s.applyDefaults(req)
	if err := s.validate(req); err != nil {
		return nil, err
	}

	// Host uniqueness: a host is owned by one deployment.
	if err := s.checkHostOwnership(ctx, req); err != nil {
		return nil, err
	}

	logger := slog.With("deploymentId", req.ID, "image", req.Image)
	if err := s.orchestrator.Apply(ctx, req); err != nil {
		logger.Error("Deployment apply failed", "error", err)
		return nil, err
	}
	logger.Info("Deployment applied", "host", req.Host)

	return s.Get(ctx, req.ID)
}

// Get returns the status of a deployment, with its public URL filled in.
func (s *Service) Get(ctx context.Context, id string) (*StatusResponse, error) {
	status, err := s.orchestrator.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	s.fillURL(ctx, status)
	return status, nil
}

// List returns all deployments.
func (s *Service) List(ctx context.Context) (*ListResponse, error) {
	statuses, err := s.orchestrator.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		s.fillURL(ctx, &statuses[i])
	}
	return &ListResponse{Deployments: statuses}, nil
}

// Delete tears down a deployment.
func (s *Service) Delete(ctx context.Context, id string) error {
	logger := slog.With("deploymentId", id)
	if err := s.orchestrator.Delete(ctx, id); err != nil {
		logger.Error("Deployment deletion failed", "error", err)
		return err
	}
	logger.Info("Deployment deleted")
	return nil
}

// Resolve maps a request host to the deployment that owns it. Used by the
// activator's router.
func (s *Service) Resolve(ctx context.Context, host string) (*Request, error) {
	statuses, err := s.orchestrator.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		spec, err := s.orchestrator.Spec(ctx, statuses[i].ID)
		if err != nil {
			continue
		}
		if spec.Host == host {
			return spec, nil
		}
	}
	return nil, apperrors.NotFound("deployment", host)
}

// Endpoints exposes ready proxy endpoints for the activator.
func (s *Service) Endpoints(ctx context.Context, id string) ([]*url.URL, error) {
	return s.orchestrator.Endpoints(ctx, id)
}

// Scale sets the deployment's replica count. Used by the activator's cold
// raise (0→replicas) and the idle-to-zero loop (→0).
func (s *Service) Scale(ctx context.Context, id string, replicas int) error {
	return s.orchestrator.Scale(ctx, id, replicas)
}

// SetTraffic validates and applies a traffic table — canary, blue-green, or
// rollback are all weight edits across existing revisions.
func (s *Service) SetTraffic(ctx context.Context, id string, targets []Target) (*StatusResponse, error) {
	if len(targets) == 0 {
		return nil, apperrors.Validation("traffic", "at least one target is required")
	}
	sum := 0
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.RevisionName == "" {
			return nil, apperrors.Validation("traffic.revisionName", "revision name is required")
		}
		if seen[t.RevisionName] {
			return nil, apperrors.Validation("traffic.revisionName", fmt.Sprintf("duplicate revision %q", t.RevisionName))
		}
		seen[t.RevisionName] = true
		if t.Percent < 0 || t.Percent > 100 {
			return nil, apperrors.Validation("traffic.percent", "percent must be between 0 and 100")
		}
		sum += t.Percent
	}
	if sum != 100 {
		return nil, apperrors.Validation("traffic", fmt.Sprintf("percents must sum to 100, got %d", sum))
	}

	if err := s.orchestrator.SetTraffic(ctx, id, targets); err != nil {
		return nil, err
	}
	slog.Info("Traffic table applied", "deploymentId", id, "targets", len(targets))
	return s.Get(ctx, id)
}

func (s *Service) fillURL(ctx context.Context, status *StatusResponse) {
	spec, err := s.orchestrator.Spec(ctx, status.ID)
	if err != nil || s.urlFor == nil {
		return
	}
	status.URL = s.urlFor(spec.Host)
}

// checkHostOwnership rejects a host already owned by another deployment.
func (s *Service) checkHostOwnership(ctx context.Context, req *Request) error {
	owner, err := s.Resolve(ctx, req.Host)
	if err != nil {
		if apperrors.HTTPStatus(err) == http.StatusNotFound {
			return nil // no owner — free to claim
		}
		return err
	}
	if owner.ID != req.ID {
		return apperrors.Conflict("host", req.Host, fmt.Sprintf("host already owned by deployment %q", owner.ID))
	}
	return nil
}

func (s *Service) applyDefaults(req *Request) {
	if req.CPU <= 0 {
		req.CPU = 1
	}
	if req.Memory <= 0 {
		req.Memory = 512
	}
	if req.Replicas <= 0 {
		req.Replicas = 1
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if req.ResponseStartTimeoutSeconds <= 0 {
		req.ResponseStartTimeoutSeconds = DefaultResponseStartTimeoutSeconds
	}
	if req.ProgressDeadlineSeconds <= 0 {
		req.ProgressDeadlineSeconds = DefaultProgressDeadlineSeconds
	}
	if req.Host == "" && req.ID != "" {
		req.Host = req.ID + "." + s.domain
	}
	req.Host = strings.ToLower(req.Host)

	if req.Autoscaling != nil {
		if req.Autoscaling.MaxReplicas <= 0 {
			req.Autoscaling.MaxReplicas = max(req.Replicas, 1)
		}
		if req.Autoscaling.Target <= 0 {
			req.Autoscaling.Target = 100
		}
	}
}

func (s *Service) validate(req *Request) error {
	if req.ID == "" {
		return apperrors.Validation("id", "deployment ID is required")
	}
	if len(req.ID) > maxIDLength {
		return apperrors.Validation("id", fmt.Sprintf("deployment ID exceeds maximum length of %d", maxIDLength))
	}
	if !idPattern.MatchString(req.ID) {
		return apperrors.Validation("id", "deployment ID must be an RFC-1123 label (lowercase alphanumeric, interior hyphens)")
	}

	if req.Image == "" {
		return apperrors.Validation("image", "image is required")
	}

	if !ValidSandbox(req.Sandbox) {
		return apperrors.Validation("sandbox", fmt.Sprintf("sandbox must be one of %q, %q, %q", SandboxRunc, SandboxGvisor, SandboxKata))
	}

	if req.Port <= 0 || req.Port > 65535 {
		return apperrors.Validation("port", "port must be between 1 and 65535")
	}

	if len(req.Host) > maxHostLength {
		return apperrors.Validation("host", fmt.Sprintf("host exceeds maximum length of %d", maxHostLength))
	}
	if !hostPattern.MatchString(req.Host) {
		return apperrors.Validation("host", "host must be an RFC-1123 subdomain")
	}

	if req.CPU > maxCPU {
		return apperrors.Validation("cpu", fmt.Sprintf("CPU exceeds maximum of %d cores", maxCPU))
	}
	if req.Memory > maxMemory {
		return apperrors.Validation("memory", fmt.Sprintf("memory exceeds maximum of %d MB", maxMemory))
	}
	if req.Replicas > maxReplicas {
		return apperrors.Validation("replicas", fmt.Sprintf("replicas exceed maximum of %d", maxReplicas))
	}
	if req.Concurrency < 0 {
		return apperrors.Validation("concurrency", "concurrency must be non-negative")
	}
	if req.Autoscaling != nil {
		a := req.Autoscaling
		if a.MinReplicas < 0 {
			return apperrors.Validation("autoscaling.minReplicas", "minReplicas must be non-negative")
		}
		if a.MaxReplicas < a.MinReplicas {
			return apperrors.Validation("autoscaling.maxReplicas", "maxReplicas must be >= minReplicas")
		}
		if a.MaxReplicas > maxReplicas {
			return apperrors.Validation("autoscaling.maxReplicas", fmt.Sprintf("maxReplicas exceeds maximum of %d", maxReplicas))
		}
		if a.Target < 0 {
			return apperrors.Validation("autoscaling.target", "target must be non-negative")
		}
	}
	if req.TimeoutSeconds > maxTimeoutSecs {
		return apperrors.Validation("timeoutSeconds", fmt.Sprintf("timeout exceeds maximum of %d seconds", maxTimeoutSecs))
	}

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

	if len(req.Artifacts) > maxArtifacts {
		return apperrors.Validation("artifacts", fmt.Sprintf("artifacts exceed maximum of %d", maxArtifacts))
	}
	for i, a := range req.Artifacts {
		if err := s.artifacts.Validate(i, a); err != nil {
			return err
		}
	}

	if req.Callback != nil && req.Callback.URL != "" {
		if err := validateURL(req.Callback.URL); err != nil {
			return apperrors.Validation("callback.url", fmt.Sprintf("invalid callback URL: %v", err))
		}
	}

	return nil
}

func validateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("malformed URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("URL must have a host")
	}
	return nil
}
