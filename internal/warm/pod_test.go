package warm

import (
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"testing"

	"orchestrator/internal/kube"
	"orchestrator/pkg/deployment"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mapperPool is testPool plus the resource and env fields buildPod maps.
func mapperPool() *pool.Pool {
	p := testPool("std")
	p.CPU = 0.5
	p.Memory = 256
	p.Environment = map[string]string{"B": "2", "A": "1"}
	return &p
}

// testBuilder is a Manager wired for pod-shape assertions only.
func testBuilder(t *testing.T, tune ...func(*Config)) *Manager {
	t.Helper()
	cfg := Config{SidecarImage: "sidecar:latest", ShimImage: "shim:latest", RunAsUser: 65532, Naming: testNaming}
	for _, f := range tune {
		f(&cfg)
	}
	return New(fake.NewClientset(), nil, cfg)
}

func TestBuildPod_Shape(t *testing.T) {
	t.Parallel()
	pod := testBuilder(t).buildPod(mapperPool(), "pool-std-aabbc", "aabbccdd")

	if pod.Name != "pool-std-aabbc" {
		t.Errorf("want the caller-chosen name (the claim token derives from it), got %q", pod.Name)
	}
	if pod.Labels[LabelManagedBy] != testNaming.ManagedBy || pod.Labels[testNaming.Pool] != "std" {
		t.Errorf("labels: got %v", pod.Labels)
	}
	if pod.Labels[testNaming.Claim] != "" {
		t.Error("a warm pod must not carry an activation label")
	}
	if len(pod.Annotations) != 0 {
		t.Errorf("a warm pod must carry no annotations (tokens are derived, never stored): got %v", pod.Annotations)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy: want Never, got %s", pod.Spec.RestartPolicy)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("service account token must not be mounted")
	}
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("want workspace + tmp volumes, got %v", pod.Spec.Volumes)
	}

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("want 2 init containers, got %d", len(pod.Spec.InitContainers))
	}
	install := pod.Spec.InitContainers[0]
	if install.Name != ContainerShimInstall || install.Image != "shim:latest" {
		t.Errorf("shim-install: got %s/%s", install.Name, install.Image)
	}
	if len(install.Args) != 2 || install.Args[0] != "-install" || install.Args[1] != shimPath {
		t.Errorf("shim-install args: got %v", install.Args)
	}
	if install.RestartPolicy != nil {
		t.Error("shim-install must be a plain init container (run to completion)")
	}

	sidecar := pod.Spec.InitContainers[1]
	if sidecar.Name != ContainerProxy || sidecar.Image != "sidecar:latest" {
		t.Errorf("proxy: got %s/%s", sidecar.Name, sidecar.Image)
	}
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("proxy must be a native sidecar (restartPolicy Always)")
	}
	env := map[string]string{}
	for _, e := range sidecar.Env {
		env[e.Name] = e.Value
	}
	if env[proxy.EnvClaimToken] != "aabbccdd" || env[envSharedVolume] != workspacePath || env[proxy.EnvTargetHost] != "127.0.0.1" {
		t.Errorf("proxy env: got %v", env)
	}
	if len(sidecar.Ports) != 2 || sidecar.Ports[0].ContainerPort != proxy.DefaultProxyPort || sidecar.Ports[1].ContainerPort != proxy.DefaultAdminPort {
		t.Errorf("proxy ports: got %v", sidecar.Ports)
	}
	probe := sidecar.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/ready" || probe.HTTPGet.Port.IntValue() != int(proxy.DefaultAdminPort) {
		t.Errorf("proxy readiness probe (the warm-ready gate): got %+v", probe)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(pod.Spec.Containers))
	}
	workload := pod.Spec.Containers[0]
	if workload.Name != ContainerWorkload || workload.Image != "runtime:latest" {
		t.Errorf("workload: got %s/%s", workload.Name, workload.Image)
	}
	if len(workload.Command) != 1 || workload.Command[0] != shimPath {
		t.Errorf("workload command must be the shim, got %v", workload.Command)
	}
	if workload.WorkingDir != workspacePath {
		t.Errorf("workload workdir: got %q", workload.WorkingDir)
	}
	if len(workload.Env) != 2 || workload.Env[0].Name != "A" || workload.Env[1].Name != "B" {
		t.Errorf("workload env (sorted): got %v", workload.Env)
	}
	if len(workload.VolumeMounts) != 2 {
		t.Errorf("workload mounts (workspace + tmp): got %v", workload.VolumeMounts)
	}
}

func TestBuildPod_Tolerations(t *testing.T) {
	t.Parallel()
	builder := testBuilder(t, func(c *Config) {
		c.Tolerations = []corev1.Toleration{{Key: "workload", Value: "edge-builds", Effect: corev1.TaintEffectNoSchedule}}
	})
	got := builder.buildPod(mapperPool(), "pool-std-x", "tok").Spec.Tolerations
	if len(got) != 1 || got[0].Key != "workload" {
		t.Errorf("tolerations: want workload=edge-builds:NoSchedule, got %+v", got)
	}
}

func TestBuildPod_Resources(t *testing.T) {
	t.Parallel()
	pod := testBuilder(t).buildPod(mapperPool(), "pool-std-x", "tok")
	res := pod.Spec.Containers[0].Resources

	if got := res.Requests.Cpu().MilliValue(); got != 500 {
		t.Errorf("cpu request: want 500m, got %dm", got)
	}
	if _, capped := res.Limits[corev1.ResourceCPU]; capped {
		t.Error("workload must have NO cpu limit (CFS-throttling)")
	}
	memRequest, memLimit := res.Requests[corev1.ResourceMemory], res.Limits[corev1.ResourceMemory]
	if memRequest.Value() != 256<<20 || memLimit.Value() != 256<<20 {
		t.Errorf("memory request=limit: want 256Mi/256Mi, got %s/%s", memRequest.String(), memLimit.String())
	}

	// A bare pool keeps no workload resources at all.
	bare := testBuilder(t).buildPod(&pool.Pool{ID: "bare", Image: "img"}, "pool-bare-x", "tok")
	bareRes := bare.Spec.Containers[0].Resources
	if len(bareRes.Requests) != 0 || len(bareRes.Limits) != 0 {
		t.Errorf("bare pool resources: got %+v", bareRes)
	}
}

func TestBuildPod_SecurityFloor(t *testing.T) {
	t.Parallel()
	pod := testBuilder(t).buildPod(mapperPool(), "pool-std-x", "tok")
	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	for _, c := range containers {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("%s: no security context", c.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
			t.Errorf("%s: want non-root 65532, got %+v", c.Name, sc)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: privilege escalation must be off", c.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: want all capabilities dropped", c.Name)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: want RuntimeDefault seccomp", c.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: want read-only rootfs", c.Name)
		}
	}
}

func TestBuildPod_RuntimeClass(t *testing.T) {
	t.Parallel()
	builder := testBuilder(t, func(c *Config) {
		c.RuntimeClasses, _ = kube.ParseRuntimeClasses("kata=kata-qemu")
	})

	for tier, want := range map[string]string{
		"":                            "",
		deployment.RuntimeClassRunc:   "",
		deployment.RuntimeClassGvisor: "gvisor",
		deployment.RuntimeClassKata:   "kata-qemu",
	} {
		p := mapperPool()
		p.RuntimeClass = tier
		got := builder.buildPod(p, "pool-std-1", "token").Spec.RuntimeClassName
		switch {
		case want == "" && got != nil:
			t.Errorf("tier %q: want no runtimeClassName, got %q", tier, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("tier %q: want runtimeClassName %q, got %v", tier, want, got)
		}
	}
}

// A consumer that declares an Agent gets an extra init container that copies the
// binary out of the image publishing it — how a sandbox pool serves the contract
// from an image that implements nothing.
func TestBuildPod_AgentInstall(t *testing.T) {
	t.Parallel()
	builder := testBuilder(t, func(c *Config) {
		c.Agent = Agent{
			Image:  "ghcr.io/open-runtimes/sandbox:0.1.0",
			Source: "/usr/local/bin/sandbox",
			Dest:   "/workspace/.sandbox-agent",
		}
	})
	pod := builder.buildPod(mapperPool(), "sbx-py-aabbc", "tok")

	if len(pod.Spec.InitContainers) != 3 {
		t.Fatalf("want shim-install, agent-install, proxy; got %d", len(pod.Spec.InitContainers))
	}
	agent := pod.Spec.InitContainers[1]
	if agent.Name != ContainerAgentInstall || agent.Image != "ghcr.io/open-runtimes/sandbox:0.1.0" {
		t.Errorf("agent-install: got %s/%s", agent.Name, agent.Image)
	}
	// Plain cp: no shell, no mkdir, so the publishing image needs to contain
	// nothing but the binary and a cp.
	if got := agent.Command; len(got) != 3 || got[0] != "cp" || got[1] != "/usr/local/bin/sandbox" || got[2] != "/workspace/.sandbox-agent" {
		t.Errorf("agent-install command: got %v", got)
	}
	if agent.RestartPolicy != nil {
		t.Error("agent-install must be a plain init container (run to completion)")
	}
	if len(agent.VolumeMounts) != 1 || agent.VolumeMounts[0].MountPath != workspacePath {
		t.Errorf("agent-install must mount the workspace: got %v", agent.VolumeMounts)
	}
	if agent.SecurityContext == nil || agent.SecurityContext.RunAsNonRoot == nil || !*agent.SecurityContext.RunAsNonRoot {
		t.Error("agent-install must run under the hardening floor like every other container")
	}

	// Deployment pools declare no agent and keep two init containers.
	plain := testBuilder(t).buildPod(mapperPool(), "pool-std-aabbc", "tok")
	if len(plain.Spec.InitContainers) != 2 {
		t.Errorf("without an Agent: want 2 init containers, got %d", len(plain.Spec.InitContainers))
	}
}
