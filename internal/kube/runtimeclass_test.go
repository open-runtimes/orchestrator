package kube

import (
	"testing"
)

func TestParseRuntimeClasses(t *testing.T) {
	t.Parallel()

	// Empty value → the defaults.
	classes, err := ParseRuntimeClasses("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if classes["gvisor"] != "gvisor" || classes["kata"] != "kata" {
		t.Errorf("defaults: got %v", classes)
	}

	// Overrides win, unmentioned tiers keep their default; empty segments
	// and whitespace are tolerated.
	classes, err = ParseRuntimeClasses(" kata=kata-qemu, ,")
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if classes["kata"] != "kata-qemu" || classes["gvisor"] != "gvisor" {
		t.Errorf("override: got %v", classes)
	}

	// Malformed entries and non-mappable tiers error.
	for _, raw := range []string{"gvisor", "gvisor=", "=gvisor", "runc=runc", "firecracker=fc"} {
		if _, err := ParseRuntimeClasses(raw); err == nil {
			t.Errorf("%q: want error, got nil", raw)
		}
	}
}

func TestRuntimeClassFor(t *testing.T) {
	t.Parallel()
	classes, _ := ParseRuntimeClasses("kata=kata-qemu")

	// runc and the empty default stamp nothing.
	if got := RuntimeClassFor(classes, ""); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := RuntimeClassFor(classes, "runc"); got != "" {
		t.Errorf("runc: got %q", got)
	}
	if got := RuntimeClassFor(classes, "gvisor"); got != "gvisor" {
		t.Errorf("gvisor: got %q", got)
	}
	if got := RuntimeClassFor(classes, "kata"); got != "kata-qemu" {
		t.Errorf("kata: got %q", got)
	}
}
