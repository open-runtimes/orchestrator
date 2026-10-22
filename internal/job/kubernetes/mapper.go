package kubernetes

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/job"
	"orchestrator/internal/kube"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/workload"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelManagedBy = "managed-by"
	LabelJobID     = "job.id"
	ManagedByValue = "jobs-service"

	AnnotationCallbackURL    = "job.callback.url"
	AnnotationCallbackKey    = "job.callback.key"
	AnnotationCallbackEvents = "job.callback.events"
	AnnotationMeta           = "job.meta"

	ContainerArtifactPre = "artifact-pre"
	ContainerWorker      = "worker"
	ContainerSidecar     = "sidecar"
	VolumeWorkspace      = "workspace"
)

// watchConfig holds the per-job values a watcher needs to emit callbacks.
// Namespace and K8s Job name are fixed / derivable (single namespace per
// orchestrator; job name is jobNameFor(jobID)) so they're not carried here.
type watchConfig struct {
	jobID string
	image string
	dest  *job.CallbackDest
}

func callbackDestFromAnnotations(ann map[string]string) *job.CallbackDest {
	url := ann[AnnotationCallbackURL]
	if url == "" {
		return nil
	}
	var meta map[string]string
	if raw := ann[AnnotationMeta]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			slog.Warn("Ignoring undecodable meta annotation", "error", err)
		}
	}
	var events []string
	if raw := ann[AnnotationCallbackEvents]; raw != "" {
		events = strings.Split(raw, ",")
	}
	return &job.CallbackDest{
		Meta:   meta,
		URL:    url,
		Key:    ann[AnnotationCallbackKey],
		Events: events,
	}
}

func jobNameFor(jobID string) string {
	return "job-" + jobID
}

// buildJob maps a job.Request to a batch/v1.Job.
//
// Preparation runs once in a regular init container. The native sidecar holds
// mounts and processes outputs on SIGTERM after the worker exits.
func buildJob(req *job.Request, cfg Config, sidecarImage string) *batchv1.Job {
	workspace := req.Workspace
	if workspace == "" {
		workspace = config.DefaultWorkspace
	}

	hasMounts := artifact.HasMount(req.Artifacts)
	// The workspace token is minted here, once per pod, and handed to the
	// agent (as a volume attribute) and to the sidecar (as env) — never to the
	// worker.
	agentMounts := hasMounts && cfg.MountMode == MountModeAgent
	var agentToken string
	if agentMounts {
		agentToken = newWorkspaceToken()
	}

	labels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelJobID:     req.ID,
	}
	annotations := jobAnnotations(req)

	ttl := int32(cfg.JobRetention.Seconds())
	backoffLimit := int32(0)
	parallelism := int32(1)
	completions := int32(1)
	grace := cfg.TerminationGracePeriodSeconds

	alwaysRestart := corev1.ContainerRestartPolicyAlways

	// Base workspace mount, shared by all containers. When the job mounts an
	// image, either the resident sidecar establishes the mount and propagates it
	// outward (Bidirectional, which requires a privileged container), or, in
	// agent mode, the node mount agent establishes it from the host; the worker
	// receives it either way (HostToContainer). A startup probe gates the worker
	// until mounts are ready. Preparation completes in the preceding init container.
	sidecarMounts := []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspace}}
	workerMounts := []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspace}}

	// Persistent volumes attach to the worker only — the sidecars operate on the
	// workspace, not the user's storage.
	pvVolumes, pvMounts := kube.PersistentVolumes(req.Volumes)
	workerMounts = append(workerMounts, pvMounts...)

	var sidecarSecurityContext *corev1.SecurityContext
	// Bound execution and resident sidecar mount retries.
	deadline := int64(req.TimeoutSeconds)
	if deadline <= 0 {
		deadline = 1800 // same default as job.Service
	}
	var startupProbe *corev1.Probe
	if hasMounts {
		startupProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/ko-app/job-sidecar", "-check-mounts"}},
			},
			PeriodSeconds:    1,
			FailureThreshold: int32(deadline),
		}
		hostToContainer := corev1.MountPropagationHostToContainer
		workerMounts[0].MountPropagation = &hostToContainer
		if agentMounts {
			// The node agent mounts inside the workspace from the host. Both
			// containers only receive: the sidecar to see what it syncs and what
			// post-job artifacts read, the worker to see its inputs. Neither
			// needs privilege; the sidecar keeps file access for that same work.
			sidecarMounts[0].MountPropagation = &hostToContainer
			sidecarSecurityContext = kube.FileAccessSecurityContext()
		} else {
			bidirectional := corev1.MountPropagationBidirectional
			sidecarMounts[0].MountPropagation = &bidirectional
			// Root + privileged: the sidecar image is distroless nonroot, but
			// mounting needs to open /dev/loop-control (root:disk 0660) and call
			// mount(2).
			sidecarSecurityContext = kube.MountingSecurityContext()
		}
	} else if _, postJob := artifact.Partition(req.Artifacts); len(postJob) > 0 {
		// Post-job artifacts read worker output and write where preparation did,
		// whatever the owner or mode of either.
		sidecarSecurityContext = kube.FileAccessSecurityContext()
	}

	var cmd []string
	if req.Command != "" {
		cmd = []string{"/bin/sh", "-c", req.Command}
	}

	sidecarPull := corev1.PullPolicy(cfg.SidecarImagePullPolicy)
	workerPull := corev1.PullPolicy(cfg.WorkerImagePullPolicy)

	// Neither the sidecar nor the worker needs the K8s API; without a mounted
	// SA token the worker also can't read the pod's annotations (callback key).
	automount := false

	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		ServiceAccountName:            cfg.ServiceAccount,
		AutomountServiceAccountToken:  &automount,
		Tolerations:                   cfg.Tolerations,
		NodeSelector:                  cfg.NodeSelector,
		TerminationGracePeriodSeconds: &grace,
		Volumes: append([]corev1.Volume{
			{Name: VolumeWorkspace, VolumeSource: workspaceSource(cfg, agentToken)},
		}, pvVolumes...),
		InitContainers: []corev1.Container{
			{
				Name:            ContainerArtifactPre,
				Image:           sidecarImage,
				ImagePullPolicy: sidecarPull,
				Args:            []string{"-mode=pre"},
				Env:             sidecarEnv(req, cfg.ArtifactEndpoint, workspace),
				VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspace}},
			},
			{
				Name:            ContainerSidecar,
				Image:           sidecarImage,
				ImagePullPolicy: sidecarPull,
				Args:            []string{"-mode=post"},
				Env:             append(sidecarEnv(req, cfg.ArtifactEndpoint, workspace), agentEnv(cfg, agentToken)...),
				VolumeMounts:    sidecarMounts,
				RestartPolicy:   &alwaysRestart,
				SecurityContext: sidecarSecurityContext,
				StartupProbe:    startupProbe,
			},
		},
		Containers: []corev1.Container{
			{
				Name:            ContainerWorker,
				Image:           req.Image,
				ImagePullPolicy: workerPull,
				Command:         cmd,
				Env:             workerEnv(req),
				WorkingDir:      workspace,
				VolumeMounts:    workerMounts,
				Resources:       cfg.Overcommit.WorkerResources(req.CPU, req.Memory),
				SecurityContext: kube.WorkloadSecurityContext(),
			},
		},
	}

	for _, s := range cfg.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobNameFor(req.ID),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &deadline,
			Parallelism:             &parallelism,
			Completions:             &completions,
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: podSpec,
			},
		},
	}
}

// workspaceSource is the job's shared workspace: an emptyDir, or, when the node
// agent performs the mounts, an inline volume the agent serves and can mount
// into from the host.
func workspaceSource(cfg Config, agentToken string) corev1.VolumeSource {
	if agentToken == "" {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	return corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:           cfg.MountAgentDriver,
		VolumeAttributes: map[string]string{mountagent.AttributeToken: agentToken},
	}}
}

// agentEnv points the sidecar at the agent on its own node. The agent listens
// on the node's address, which only the downward API knows.
func agentEnv(cfg Config, agentToken string) []corev1.EnvVar {
	if agentToken == "" {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{Name: mountagent.EnvEndpoint, Value: "http://$(HOST_IP):" + strconv.Itoa(cfg.MountAgentPort)},
		{Name: mountagent.EnvToken, Value: agentToken},
	}
}

// newWorkspaceToken returns 32 random bytes, hex-encoded: the capability a
// sidecar presents to the agent for its own workspace.
func newWorkspaceToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

func jobAnnotations(req *job.Request) map[string]string {
	annotations := map[string]string{}
	if req.Callback != nil && req.Callback.URL != "" {
		annotations[AnnotationCallbackURL] = req.Callback.URL
		if req.Callback.Key != "" {
			annotations[AnnotationCallbackKey] = req.Callback.Key
		}
		if len(req.Callback.Events) > 0 {
			annotations[AnnotationCallbackEvents] = strings.Join(req.Callback.Events, ",")
		}
	}
	if len(req.Meta) > 0 {
		if metaJSON, err := json.Marshal(req.Meta); err == nil {
			annotations[AnnotationMeta] = string(metaJSON)
		}
	}
	return annotations
}

func workerEnv(req *job.Request) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(req.Environment))
	for k, v := range req.Environment {
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

func sidecarEnv(req *job.Request, artifactEndpoint, workspace string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "JOB_ID", Value: req.ID},
		{Name: config.EnvSharedVolume, Value: workspace},
		{Name: "TIMEOUT_SECONDS", Value: strconv.Itoa(req.TimeoutSeconds)},
	}
	// MarshalArtifacts injects each artifact's "type" field, which the sidecar
	// needs to unmarshal them back into concrete types.
	if artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts); err == nil {
		env = append(env, corev1.EnvVar{Name: workload.EnvArtifacts, Value: string(artifactsJSON)})
	}
	if artifactEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: "ARTIFACT_ENDPOINT", Value: artifactEndpoint})
	}
	if req.ArtifactToken != "" {
		env = append(env, corev1.EnvVar{Name: "ARTIFACT_TOKEN", Value: req.ArtifactToken})
	}
	for _, kv := range config.LoadS3Credentials().ToEnv() {
		env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	if req.Callback != nil && req.Callback.URL != "" {
		env = append(env, corev1.EnvVar{Name: "CALLBACK_URL", Value: req.Callback.URL})
		if req.Callback.Key != "" {
			env = append(env, corev1.EnvVar{Name: "CALLBACK_KEY", Value: req.Callback.Key})
		}
		if len(req.Callback.Events) > 0 {
			env = append(env, corev1.EnvVar{Name: "CALLBACK_EVENTS", Value: strings.Join(req.Callback.Events, ",")})
		}
	}
	if len(req.Meta) > 0 {
		if metaJSON, err := json.Marshal(req.Meta); err == nil {
			env = append(env, corev1.EnvVar{Name: "JOB_META", Value: string(metaJSON)})
		}
	}
	return env
}
