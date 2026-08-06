package docker

import (
	"orchestrator/internal/sandbox"
	"testing"
)

// The grammar itself is pkg/sandbox's (host_test.go); what this backend owns is
// that the data listener IS the edge, so its port rides in the URL — unlike
// Kubernetes, where a gateway fronts port 80.
func TestAddressing_DataPortRidesInTheURL(t *testing.T) {
	t.Parallel()
	cfg := Config{SandboxDomain: "sandboxes.test", Scheme: "http", DataPort: "8081"}
	addr := cfg.addressing()
	if got, want := addr.URL("abc"), "http://s-abc.sandboxes.test:8081"; got != want {
		t.Errorf("URL: want %s, got %s", want, got)
	}
	if got, want := addr.PortURL("abc", 5173), "http://s-abc-5173.sandboxes.test:8081"; got != want {
		t.Errorf("PortURL: want %s, got %s", want, got)
	}

	// Port 80 leaves the URL bare, so a fronted sandbox reads as it does on
	// Kubernetes.
	bareCfg := Config{SandboxDomain: "sandboxes.test", Scheme: "https", DataPort: "80"}
	bare := bareCfg.addressing()
	if got, want := bare.URL("abc"), "https://s-abc.sandboxes.test"; got != want {
		t.Errorf("bare URL: want %s, got %s", want, got)
	}
}

func TestVolumeLabels_CarryTheSandbox(t *testing.T) {
	t.Parallel()
	req := &sandbox.Request{ID: "agent", Pool: "py", Token: "tok", Ports: []int{5173}}
	labels := volumeLabels(req, `{"id":"agent","ports":[5173]}`)

	if labels[labelID] != "agent" || labels[labelPool] != "py" || labels[labelToken] != "tok" {
		t.Errorf("labels: got %v", labels)
	}
	// The volume is the identity anchor: its spec label must reconstruct the
	// sandbox after a restart, extra ports included.
	spec, err := parseSpec(labels[labelSpec])
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if len(spec.Ports) != 1 || spec.Ports[0] != 5173 {
		t.Errorf("spec round trip: got %+v", spec)
	}
}

func TestPortList(t *testing.T) {
	t.Parallel()
	if got := portList([]int{5173, 9229}); got != "5173,9229" {
		t.Errorf("portList: got %q", got)
	}
	if got := portList(nil); got != "" {
		t.Errorf("portList(nil): got %q", got)
	}
}
