package kubernetes

import (
	"encoding/json"
	"fmt"
	"orchestrator/pkg/job"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	ContainerWorker       = "worker"
	ContainerArtifactPre  = "artifact-pre"
	ContainerArtifactPost = "artifact-post"
	VolumeWorkspace       = "workspace"
)

// watchConfig holds the per-job values a watcher needs to emit callbacks.
// Namespace and K8s Job name are fixed / derivable (single namespace per
// orchestrator; job name is jobNameFor(jobID)) so they're not carried here.
type watchConfig struct {
	jobID string
	image string
	dest  *job.CallbackDest
}

func watchConfigFromRequest(req *job.Request) *watchConfig {
	cfg := &watchConfig{
		jobID: req.ID,
		image: req.Image,
	}
	if req.Callback != nil && req.Callback.URL != "" {
		cfg.dest = &job.CallbackDest{
			Meta:   req.Meta,
			URL:    req.Callback.URL,
			Key:    req.Callback.Key,
			Events: req.Callback.Events,
		}
	}
	return cfg
}

func watchConfigFromJob(j *batchv1.Job) *watchConfig {
	cfg := &watchConfig{
		jobID: j.Labels[LabelJobID],
	}
	for _, c := range j.Spec.Template.Spec.Containers {
		if c.Name == ContainerWorker {
			cfg.image = c.Image
			break
		}
	}
	cfg.dest = callbackDestFromAnnotations(j.Annotations)
	return cfg
}

func callbackDestFromAnnotations(ann map[string]string) *job.CallbackDest {
	url := ann[AnnotationCallbackURL]
	if url == "" {
		return nil
	}
	var meta map[string]string
	if raw := ann[AnnotationMeta]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &meta)
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
// Pod template contains:
//   - initContainer "artifact-pre": regular init, runs pre-job artifacts and exits
//   - initContainer "artifact-post": native sidecar (restartPolicy: Always), runs post-job
//     artifacts on SIGTERM (sent by kubelet when the worker exits)
//   - container "worker": the user workload
//
// All three share an emptyDir volume mounted at req.Workspace.
func buildJob(req *job.Request, cfg OrchestratorConfig, sidecarImage string) *batchv1.Job {
	workspace := req.Workspace
	if workspace == "" {
		workspace = "/workspace"
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

	volumeMounts := []corev1.VolumeMount{
		{Name: VolumeWorkspace, MountPath: workspace},
	}

	var cmd []string
	if req.Command != "" {
		cmd = []string{"/bin/sh", "-c", req.Command}
	}

	sidecarPull := corev1.PullPolicy(cfg.SidecarImagePullPolicy)
	workerPull := corev1.PullPolicy(cfg.WorkerImagePullPolicy)

	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		ServiceAccountName:            cfg.ServiceAccount,
		TerminationGracePeriodSeconds: &grace,
		Volumes: []corev1.Volume{
			{
				Name:         VolumeWorkspace,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
		InitContainers: []corev1.Container{
			{
				Name:            ContainerArtifactPre,
				Image:           sidecarImage,
				ImagePullPolicy: sidecarPull,
				Args:            []string{"-mode=pre"},
				Env:             sidecarEnv(req, cfg.ArtifactEndpoint, workspace),
				VolumeMounts:    volumeMounts,
			},
			{
				Name:            ContainerArtifactPost,
				Image:           sidecarImage,
				ImagePullPolicy: sidecarPull,
				Args:            []string{"-mode=post"},
				Env:             sidecarEnv(req, cfg.ArtifactEndpoint, workspace),
				VolumeMounts:    volumeMounts,
				RestartPolicy:   &alwaysRestart,
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
				VolumeMounts:    volumeMounts,
				Resources:       workerResources(req),
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

func workerResources(req *job.Request) corev1.ResourceRequirements {
	res := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{},
		Requests: corev1.ResourceList{},
	}
	if req.CPU > 0 {
		cpu := resource.MustParse(fmt.Sprintf("%.3f", req.CPU))
		res.Limits[corev1.ResourceCPU] = cpu
		res.Requests[corev1.ResourceCPU] = cpu
	}
	if req.Memory > 0 {
		mem := resource.MustParse(fmt.Sprintf("%dMi", req.Memory))
		res.Limits[corev1.ResourceMemory] = mem
		res.Requests[corev1.ResourceMemory] = mem
	}
	return res
}

func sidecarEnv(req *job.Request, artifactEndpoint, workspace string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "JOB_ID", Value: req.ID},
		{Name: "SHARED_VOLUME_PATH", Value: workspace},
		{Name: "TIMEOUT_SECONDS", Value: strconv.Itoa(req.TimeoutSeconds)},
	}
	if artifactsJSON, err := json.Marshal(req.Artifacts); err == nil {
		env = append(env, corev1.EnvVar{Name: "ARTIFACTS_JSON", Value: string(artifactsJSON)})
	}
	if artifactEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: "ARTIFACT_ENDPOINT", Value: artifactEndpoint})
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
