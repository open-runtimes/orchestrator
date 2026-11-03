//go:build k8s_integration

package kubernetes

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// The checks the user-namespaced worker runs on a writable image mount, each
// printed as OK or FAIL. Reads and new files work without ID mapping; the
// rest are what an unmapped image refuses.
const usernsChecks = `cd /mnt/workspace/code
r() { if sh -c "$1" >/tmp/o 2>&1; then echo "OK   $1"; else echo "FAIL $1: $(cat /tmp/o)"; fi; }
r 'test "$(stat -c %u:%g hello.txt)" = 0:0'
r 'cat hello.txt'
r 'echo new > new.txt'
r 'echo more >> hello.txt'
r 'chmod 600 dir/data'
r 'rm dir/data'
r 'mkdir dir/sub'
`

// TestIntegration_AgentMountInUserNamespace runs a `hostUsers: false` pod
// through the parts no kernel test reaches: kubelet's record of the pod's
// mapping, containerd's ID-mapped bind of the agent's workspace, and the
// agent's mount propagating into a user-namespaced container. The sidecar asks
// the agent for a writable mount of an erofs image, and the worker, root of
// its own namespace, edits it.
func TestIntegration_AgentMountInUserNamespace(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()
	ctx := t.Context()
	pods, configMaps := o.client.CoreV1().Pods(testNamespace), o.client.CoreV1().ConfigMaps(testNamespace)

	img := filepath.Join(t.TempDir(), "code.erofs")
	testutil.WriteErofs(t, img)
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("userns-%d", time.Now().UnixNano()%1_000_000_000)
	cm := &corev1.ConfigMap{Name: name, BinaryData: map[string][]byte{"code.erofs": data}}
	if _, err := configMaps.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer configMaps.Delete(ctx, name, metav1.DeleteOptions{})

	token := rand.Text()
	mount := fmt.Sprintf(`cp /img/code.erofs /workspace/ && wget -q -O- --header "Authorization: Bearer $%s" --header "Content-Type: application/json" `+
		`--post-data '{"source":"code.erofs","target":"code","writable":true,"sizeMiB":16}' "$%s/v1/mount" && sleep 3600`,
		mountagent.EnvToken, mountagent.EnvEndpoint)
	hardened := func(caps ...corev1.Capability) *corev1.SecurityContext {
		return &corev1.SecurityContext{
			RunAsUser:                ptr.To[int64](0),
			AllowPrivilegeEscalation: new(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: caps},
		}
	}
	pod := &corev1.Pod{
		Name: name,
		Spec: corev1.PodSpec{
			HostUsers:                     new(false),
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  new(false),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			Containers: []corev1.Container{
				{
					Name:    "sidecar",
					Image:   "alpine:3.20",
					Command: []string{"sh", "-c", mount},
					Env: []corev1.EnvVar{
						{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
						{Name: mountagent.EnvEndpoint, Value: fmt.Sprintf("http://$(HOST_IP):%d", mountagent.DefaultPort)},
						{Name: mountagent.EnvToken, Value: token},
					},
					SecurityContext: hardened("DAC_OVERRIDE"),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace", MountPropagation: ptr.To(corev1.MountPropagationHostToContainer)},
						{Name: "img", MountPath: "/img"},
					},
				},
				{
					// The whole workspace, never a subPath: runc ID-maps a
					// subPath bind, which fails on the agent's already
					// mapped mount.
					Name:            "worker",
					Image:           "alpine:3.20",
					Command:         []string{"sh", "-c", "until [ -f /mnt/workspace/code/hello.txt ]; do sleep 0.2; done\n" + usernsChecks},
					SecurityContext: hardened(),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/mnt/workspace", ReadOnly: true, MountPropagation: ptr.To(corev1.MountPropagationHostToContainer)},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "workspace", CSI: &corev1.CSIVolumeSource{
					Driver: mountagent.Driver, VolumeAttributes: map[string]string{mountagent.AttributeToken: token},
				}},
				{Name: "img", ConfigMap: &corev1.ConfigMapVolumeSource{
					Name: name,
				}},
			},
		},
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Deleting must not hang on the agent's mounts: kubelet unpublishes the
		// workspace only once the agent has taken them down.
		_ = pods.Delete(ctx, name, metav1.DeleteOptions{})
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
			if _, err := pods.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				return
			}
		}
		t.Errorf("pod %s still exists a minute after deletion", name)
	}()

	// The sidecar sleeps once it has mounted, so its exit is a failure too.
	exited := waitForExit(t, o, name, 3*time.Minute)
	logs := map[string][]byte{}
	for _, c := range []string{"sidecar", "worker"} {
		logs[c], _ = pods.GetLogs(name, &corev1.PodLogOptions{Container: c}).DoRaw(ctx)
		t.Logf("%s:\n%s", c, logs[c])
	}
	worker := exited["worker"]
	if worker == nil || worker.ExitCode != 0 || bytes.Contains(logs["worker"], []byte("FAIL")) || strings.Count(string(logs["worker"]), "OK ") != 7 {
		t.Fatalf("the user-namespaced worker could not use its mount: %+v", exited)
	}
	created, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hu := created.Spec.HostUsers; hu == nil || *hu {
		t.Fatal("the pod did not run in a user namespace")
	}
}

// waitForExit waits for any container in pod to exit and returns those that
// have, by name.
func waitForExit(t *testing.T, o *Orchestrator, pod string, timeout time.Duration) map[string]*corev1.ContainerStateTerminated {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		p, err := o.client.CoreV1().Pods(testNamespace).Get(t.Context(), pod, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		exited := map[string]*corev1.ContainerStateTerminated{}
		for _, s := range p.Status.ContainerStatuses {
			if s.State.Terminated != nil {
				exited[s.Name] = s.State.Terminated
			}
		}
		if len(exited) > 0 {
			return exited
		}
	}
	t.Fatalf("no container in %s finished within %s", pod, timeout)
	return nil
}
