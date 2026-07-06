package kube

import (
	"fmt"
	"orchestrator/pkg/deployment"
	"strings"
)

// ParseSandboxRuntimeClasses parses the KUBE_SANDBOX_RUNTIME_CLASSES value —
// comma-separated "tier=runtimeClass" pairs, e.g. "gvisor=gvisor,kata=kata-qemu"
// — over the defaults (gvisor→gvisor, kata→kata). Empty segments are
// tolerated; runc never maps: it is the cluster's default runtime, so no
// runtimeClassName is stamped for it.
func ParseSandboxRuntimeClasses(raw string) (map[string]string, error) {
	classes := map[string]string{
		deployment.SandboxGvisor: deployment.SandboxGvisor,
		deployment.SandboxKata:   deployment.SandboxKata,
	}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		tier, class, ok := strings.Cut(entry, "=")
		if !ok || tier == "" || class == "" {
			return nil, fmt.Errorf("invalid KUBE_SANDBOX_RUNTIME_CLASSES entry %q (want tier=runtimeClass)", entry)
		}
		if tier != deployment.SandboxGvisor && tier != deployment.SandboxKata {
			return nil, fmt.Errorf("invalid KUBE_SANDBOX_RUNTIME_CLASSES tier %q (only %q and %q map to a RuntimeClass; %q is the cluster default)",
				tier, deployment.SandboxGvisor, deployment.SandboxKata, deployment.SandboxRunc)
		}
		classes[tier] = class
	}
	return classes, nil
}

// RuntimeClassFor resolves a sandbox tier to its RuntimeClass name; "" for
// the runc/default tier, which stamps no runtimeClassName.
func RuntimeClassFor(classes map[string]string, sandbox string) string {
	if sandbox == "" || sandbox == deployment.SandboxRunc {
		return ""
	}
	return classes[sandbox]
}
