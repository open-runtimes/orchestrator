package deployment

import (
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/volume"
	"testing"
)

func TestValidate_RuntimeClass(t *testing.T) {
	t.Parallel()
	s := &Service{artifacts: artifact.ServingRegistry(), domain: "example.com"}

	for _, tier := range []string{"", RuntimeClassRunc, RuntimeClassGvisor, RuntimeClassKata} {
		req := &Request{ID: "app", Image: "nginx", Port: 8080, RuntimeClass: tier}
		s.applyDefaults(req)
		if err := s.validate(req); err != nil {
			t.Errorf("tier %q: want valid, got %v", tier, err)
		}
	}

	for _, tier := range []string{"firecracker", "Runc", "gVisor"} {
		req := &Request{ID: "app", Image: "nginx", Port: 8080, RuntimeClass: tier}
		s.applyDefaults(req)
		err := s.validate(req)
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("tier %q: want validation error, got %v", tier, err)
		}
	}
}

// Workspace: empty defaults to /workspace; an absolute path is kept; a
// relative path is rejected.
func TestValidate_Workspace(t *testing.T) {
	t.Parallel()
	s := &Service{artifacts: artifact.ServingRegistry(), domain: "example.com"}

	req := &Request{ID: "app", Image: "nginx", Port: 8080}
	s.applyDefaults(req)
	if req.Workspace != DefaultWorkspace {
		t.Errorf("default workspace = %q, want %q", req.Workspace, DefaultWorkspace)
	}

	req = &Request{ID: "app", Image: "nginx", Port: 8080, Workspace: "/usr/local/server"}
	s.applyDefaults(req)
	if err := s.validate(req); err != nil {
		t.Errorf("absolute workspace: want valid, got %v", err)
	}
	if req.Workspace != "/usr/local/server" {
		t.Errorf("workspace overwritten: got %q", req.Workspace)
	}

	req = &Request{ID: "app", Image: "nginx", Port: 8080, Workspace: "relative/dir"}
	s.applyDefaults(req)
	if err := s.validate(req); !errors.Is(err, apperrors.ErrValidation) {
		t.Errorf("relative workspace: want validation error, got %v", err)
	}
}

// A workspace, the reserved /tmp mount, and each user volume must occupy
// distinct mount targets — colliding targets emit duplicate Docker/K8s mounts.
func TestValidate_WorkspaceCollision(t *testing.T) {
	t.Parallel()
	s := &Service{artifacts: artifact.ServingRegistry(), domain: "example.com"}

	vol := func(p string) volume.Volume { return volume.Volume{Source: "data", Path: p} }

	rejected := map[string]*Request{
		"workspace is reserved /tmp":     {ID: "app", Image: "nginx", Port: 8080, Workspace: "/tmp"},
		"workspace equals a user volume": {ID: "app", Image: "nginx", Port: 8080, Workspace: "/data", Volumes: []volume.Volume{vol("/data")}},
		"default workspace vs volume":    {ID: "app", Image: "nginx", Port: 8080, Volumes: []volume.Volume{vol("/workspace")}},
		"user volume claims /tmp":        {ID: "app", Image: "nginx", Port: 8080, Volumes: []volume.Volume{vol("/tmp")}},
		"two volumes same path":          {ID: "app", Image: "nginx", Port: 8080, Volumes: []volume.Volume{vol("/data"), vol("/data")}},
	}
	for name, req := range rejected {
		s.applyDefaults(req)
		if err := s.validate(req); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}

	ok := &Request{ID: "app", Image: "nginx", Port: 8080, Workspace: "/usr/local/server", Volumes: []volume.Volume{vol("/data")}}
	s.applyDefaults(ok)
	if err := s.validate(ok); err != nil {
		t.Errorf("distinct targets: want valid, got %v", err)
	}
}

// Hosts: empty derives the {id}.{domain} primary; multiple hosts validate
// individually and reject duplicates.
func TestValidate_Hosts(t *testing.T) {
	t.Parallel()
	s := &Service{artifacts: artifact.ServingRegistry(), domain: "example.com"}

	req := &Request{ID: "app", Image: "nginx", Port: 8080}
	s.applyDefaults(req)
	if len(req.Hosts) != 1 || req.Hosts[0] != "app.example.com" {
		t.Errorf("default hosts = %v, want [app.example.com]", req.Hosts)
	}

	req = &Request{ID: "app", Image: "nginx", Port: 8080, Hosts: []string{"www.acme.com", "Acme.COM"}}
	s.applyDefaults(req)
	if err := s.validate(req); err != nil {
		t.Fatalf("two hosts: %v", err)
	}
	if req.Hosts[1] != "acme.com" {
		t.Errorf("hosts not lowercased: %v", req.Hosts)
	}

	for name, hosts := range map[string][]string{
		"duplicate": {"a.acme.com", "a.acme.com"},
		"invalid":   {"not_a_host!"},
	} {
		req := &Request{ID: "app", Image: "nginx", Port: 8080, Hosts: hosts}
		s.applyDefaults(req)
		if err := s.validate(req); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
}
