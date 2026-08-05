package docker

import (
	"orchestrator/pkg/sandbox"
	"testing"
)

func TestURLs_TokenAddressesEveryPort(t *testing.T) {
	t.Parallel()
	cfg := Config{SandboxDomain: "sandboxes.test", Scheme: "http", DataPort: "8081"}

	// The data listener is the edge on Docker, so its port is part of the URL —
	// unlike Kubernetes, where a gateway fronts port 80.
	if got, want := cfg.URLFor("abc"), "http://s-abc.sandboxes.test:8081"; got != want {
		t.Errorf("URLFor: want %s, got %s", want, got)
	}
	if got, want := cfg.PortURLFor("abc", 5173), "http://s-abc-5173.sandboxes.test:8081"; got != want {
		t.Errorf("PortURLFor: want %s, got %s", want, got)
	}
	urls := cfg.URLsFor("abc", 3000, []int{5173})
	if urls["3000"] != cfg.URLFor("abc") || urls["5173"] != cfg.PortURLFor("abc", 5173) {
		t.Errorf("URLsFor: got %v", urls)
	}
	if cfg.URLFor("") != "" {
		t.Error("a sandbox with no token has no URL")
	}

	// Port 80 (or none) leaves the URL bare, so a fronted deployment reads
	// the same as on Kubernetes.
	bare := Config{SandboxDomain: "sandboxes.test", Scheme: "https", DataPort: "80"}
	if got, want := bare.URLFor("abc"), "https://s-abc.sandboxes.test"; got != want {
		t.Errorf("bare URLFor: want %s, got %s", want, got)
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
