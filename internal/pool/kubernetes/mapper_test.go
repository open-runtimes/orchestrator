package kubernetes

import (
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testPool() *pool.Pool {
	return &pool.Pool{
		ID:          "std",
		Image:       "runtime:latest",
		Size:        2,
		CPU:         0.5,
		Memory:      256,
		Environment: map[string]string{"B": "2", "A": "1"},
	}
}

func testConfig() Config {
	cfg := Config{SidecarImage: "sidecar:latest", ShimImage: "shim:latest", RunAsUser: 65532}
	cfg.applyDefaults()
	return cfg
}

func TestBuildWarmPod_Shape(t *testing.T) {
	t.Parallel()
	warm := buildWarmPod(testPool(), testConfig(), "aabbccdd")

	if warm.GenerateName != "pool-std-" || warm.Name != "" {
		t.Errorf("want GenerateName pool-std-, got %q/%q", warm.GenerateName, warm.Name)
	}
	if warm.Labels[LabelManagedBy] != ManagedByValue || warm.Labels[LabelPoolID] != "std" {
		t.Errorf("labels: got %v", warm.Labels)
	}
	if warm.Labels[LabelActivation] != "" {
		t.Error("a warm pod must not carry an activation label")
	}
	if warm.Annotations[AnnotationClaimToken] != "aabbccdd" {
		t.Errorf("claim token annotation: got %v", warm.Annotations)
	}
	if warm.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy: want Never, got %s", warm.Spec.RestartPolicy)
	}
	if warm.Spec.AutomountServiceAccountToken == nil || *warm.Spec.AutomountServiceAccountToken {
		t.Error("service account token must not be mounted")
	}
	if len(warm.Spec.Volumes) != 2 {
		t.Fatalf("want workspace + tmp volumes, got %v", warm.Spec.Volumes)
	}

	if len(warm.Spec.InitContainers) != 2 {
		t.Fatalf("want 2 init containers, got %d", len(warm.Spec.InitContainers))
	}
	install := warm.Spec.InitContainers[0]
	if install.Name != ContainerShimInstall || install.Image != "shim:latest" {
		t.Errorf("shim-install: got %s/%s", install.Name, install.Image)
	}
	if len(install.Args) != 2 || install.Args[0] != "-install" || install.Args[1] != shimPath {
		t.Errorf("shim-install args: got %v", install.Args)
	}
	if install.RestartPolicy != nil {
		t.Error("shim-install must be a plain init container (run to completion)")
	}

	sidecar := warm.Spec.InitContainers[1]
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

	if len(warm.Spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(warm.Spec.Containers))
	}
	workload := warm.Spec.Containers[0]
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

func TestBuildWarmPod_Resources(t *testing.T) {
	t.Parallel()
	warm := buildWarmPod(testPool(), testConfig(), "tok")
	res := warm.Spec.Containers[0].Resources

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
	bare := buildWarmPod(&pool.Pool{ID: "bare", Image: "img"}, testConfig(), "tok")
	bareRes := bare.Spec.Containers[0].Resources
	if len(bareRes.Requests) != 0 || len(bareRes.Limits) != 0 {
		t.Errorf("bare pool resources: got %+v", bareRes)
	}
}

func TestBuildWarmPod_SecurityFloor(t *testing.T) {
	t.Parallel()
	warm := buildWarmPod(testPool(), testConfig(), "tok")
	containers := append(warm.Spec.InitContainers, warm.Spec.Containers...)
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

func TestMintClaimToken(t *testing.T) {
	t.Parallel()
	token, err := mintClaimToken()
	if err != nil {
		t.Fatalf("mintClaimToken: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(token) {
		t.Errorf("want 32 hex chars, got %q", token)
	}
	second, _ := mintClaimToken()
	if token == second {
		t.Error("tokens must be random per pod")
	}
}
