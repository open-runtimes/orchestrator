package kube

import (
	"fmt"
	"orchestrator/pkg/deployment"
	"strings"
)

// ParseRuntimeClasses parses the KUBE_RUNTIME_CLASSES value —
// comma-separated "tier=runtimeClass" pairs, e.g. "gvisor=gvisor,kata=kata-qemu"
// — over the defaults (gvisor→gvisor, kata→kata). Empty segments are
// tolerated; runc never maps: it is the cluster's default runtime, so no
// runtimeClassName is stamped for it.
func ParseRuntimeClasses(raw string) (map[string]string, error) {
	classes := map[string]string{
		deployment.RuntimeClassGvisor: deployment.RuntimeClassGvisor,
		deployment.RuntimeClassKata:   deployment.RuntimeClassKata,
	}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		tier, class, ok := strings.Cut(entry, "=")
		if !ok || tier == "" || class == "" {
			return nil, fmt.Errorf("invalid KUBE_RUNTIME_CLASSES entry %q (want tier=runtimeClass)", entry)
		}
		if tier != deployment.RuntimeClassGvisor && tier != deployment.RuntimeClassKata {
			return nil, fmt.Errorf("invalid KUBE_RUNTIME_CLASSES tier %q (only %q and %q map to a RuntimeClass; %q is the cluster default)",
				tier, deployment.RuntimeClassGvisor, deployment.RuntimeClassKata, deployment.RuntimeClassRunc)
		}
		classes[tier] = class
	}
	return classes, nil
}

// RuntimeClassFor resolves an isolation tier to its RuntimeClass name; "" for
// the runc/default tier, which stamps no runtimeClassName.
func RuntimeClassFor(classes map[string]string, tier string) string {
	if tier == "" || tier == deployment.RuntimeClassRunc {
		return ""
	}
	return classes[tier]
}
