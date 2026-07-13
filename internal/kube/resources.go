package kube

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"orchestrator/internal/config"
)

// TolerationsFromEnv reads KUBE_WORKLOAD_TOLERATIONS, a JSON array in the pod
// spec's tolerations schema, stamped on every workload pod so tainted node
// pools (e.g. workload=edge-builds:NoSchedule) can host them. Empty means
// none; malformed JSON is an error — a typo must not silently strand pods on
// the wrong nodes.
func TolerationsFromEnv() ([]corev1.Toleration, error) {
	raw := config.GetEnv("KUBE_WORKLOAD_TOLERATIONS", "")
	if raw == "" {
		return nil, nil
	}
	var tolerations []corev1.Toleration
	if err := json.Unmarshal([]byte(raw), &tolerations); err != nil {
		return nil, fmt.Errorf("parse KUBE_WORKLOAD_TOLERATIONS: %w", err)
	}
	return tolerations, nil
}

// Overcommit derives a workload's scheduler requests from its declared limits
// (docs/operations.md "Resource model"). The client declares one ceiling per
// resource; the platform derives the request as limit / divisor:
//
//   - CPU: request = limit / CPU, and NO cpu limit — CFS-quota throttling at a
//     limit is a tail-latency killer; cpu requests (shares) handle fairness,
//     bursting rides idle headroom.
//   - Memory: limit as declared, request = limit / Memory. Memory is
//     incompressible — overcommitting it trades OOM kills for density, so the
//     default keeps request == limit.
//
// Divisors ≤ 0 (including the zero value) mean 1: no overcommit.
type Overcommit struct {
	CPU    float64
	Memory float64
}

// OvercommitFromEnv reads the divisors from KUBE_CPU_OVERCOMMIT and
// KUBE_MEMORY_OVERCOMMIT, defaulting each to 1.
func OvercommitFromEnv() Overcommit {
	return Overcommit{
		CPU:    config.GetFloatEnv("KUBE_CPU_OVERCOMMIT", 1),
		Memory: config.GetFloatEnv("KUBE_MEMORY_OVERCOMMIT", 1),
	}
}

// WorkerResources builds the worker container's resources from its declared
// ceilings (cores, MiB). Zero ceilings stay unset — a bare spec keeps no
// resources at all.
func (o Overcommit) WorkerResources(cpuCores float64, memoryMi int) corev1.ResourceRequirements {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	if cpuCores > 0 {
		requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(requestUnits(cpuCores*1000, o.CPU), resource.DecimalSI)
	}
	if memoryMi > 0 {
		limits[corev1.ResourceMemory] = resource.MustParse(strconv.Itoa(memoryMi) + "Mi")
		requests[corev1.ResourceMemory] = *resource.NewQuantity(requestUnits(float64(memoryMi), o.Memory)*1024*1024, resource.BinarySI)
	}
	return corev1.ResourceRequirements{Limits: limits, Requests: requests}
}

// requestUnits divides a declared limit by the overcommit divisor, rounded up
// and floored at 1 unit. Divisors ≤ 0 mean 1.
func requestUnits(limit, overcommit float64) int64 {
	if overcommit <= 0 {
		overcommit = 1
	}
	return max(int64(math.Ceil(limit/overcommit)), 1)
}
