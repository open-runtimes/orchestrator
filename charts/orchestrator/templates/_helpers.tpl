{{- define "orchestrator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "orchestrator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
  jobsName: resource name for the Deployment, Service, and RBAC that serve the
  jobs API. Deliberately separate from the release's fullname so other APIs
  (deployments, statefulsets, …) can coexist in the same chart as distinct
  Deployments. Defaults to "<release>-jobs" so multiple chart releases in the
  same namespace don't collide.
*/}}
{{- define "orchestrator.jobsName" -}}
{{- if .Values.jobs.fullnameOverride -}}
{{- .Values.jobs.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if eq .Release.Name "orchestrator" -}}
{{- "jobs" -}}
{{- else -}}
{{- printf "%s-jobs" (include "orchestrator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
  deploymentsName: resource name for the serving-plane (deployments) API,
  following the same per-component convention as jobsName.
*/}}
{{- define "orchestrator.deploymentsName" -}}
{{- if .Values.deployments.fullnameOverride -}}
{{- .Values.deployments.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if eq .Release.Name "orchestrator" -}}
{{- "deployments" -}}
{{- else -}}
{{- printf "%s-deployments" (include "orchestrator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.deploymentsImage" -}}
{{- if .Values.deployments.image.ref -}}
{{- .Values.deployments.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.image.repository (default .Chart.AppVersion .Values.deployments.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.deploymentsSidecarImage" -}}
{{- if .Values.deployments.sidecarImage.ref -}}
{{- .Values.deployments.sidecarImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.sidecarImage.repository (default .Chart.AppVersion .Values.deployments.sidecarImage.tag) -}}
{{- end -}}
{{- end -}}

{{/*
  deploymentsWorkloadNamespace: where workload pods (revisions, warm pools),
  their markers/routes, and the leader leases live. The release namespace
  unless the hardened workload namespace is enabled.
*/}}
{{- define "orchestrator.deploymentsWorkloadNamespace" -}}
{{- if .Values.deployments.workloadNamespace.enabled -}}
{{- default (printf "%s-workloads" .Release.Namespace) .Values.deployments.workloadNamespace.name -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.poolShimImage" -}}
{{- if .Values.deployments.shimImage.ref -}}
{{- .Values.deployments.shimImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.shimImage.repository (default .Chart.AppVersion .Values.deployments.shimImage.tag) -}}
{{- end -}}
{{- end -}}

{{/*
  imagePullSecrets: pull credentials for every pod this chart renders. Not for
  workload pods (job pods, deployment replicas, warm pods) — the services
  create those, so their credentials go on the ServiceAccount they run as.
*/}}
{{- define "orchestrator.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets -}}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end -}}
{{- end -}}

{{- define "orchestrator.activatorImage" -}}
{{- if .Values.deployments.activator.image.ref -}}
{{- .Values.deployments.activator.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.activator.image.repository (default .Chart.AppVersion .Values.deployments.activator.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.activatorLabels" -}}
{{ include "orchestrator.labels" . }}
app.kubernetes.io/component: deployments-activator
{{- end -}}

{{- define "orchestrator.activatorSelectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: deployments-activator
{{- end -}}

{{- define "orchestrator.deploymentsLabels" -}}
{{ include "orchestrator.labels" . }}
app.kubernetes.io/component: deployments
{{- end -}}

{{- define "orchestrator.deploymentsSelectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: deployments
{{- end -}}

{{- define "orchestrator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else -}}
{{- include "orchestrator.jobsName" . -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.jobSidecarServiceAccountName" -}}
{{- if .Values.serviceAccount.jobSidecarName -}}
{{- .Values.serviceAccount.jobSidecarName -}}
{{- else -}}
{{- printf "%s-job-sidecar" (include "orchestrator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.jobsImage" -}}
{{- if .Values.jobs.image.ref -}}
{{- .Values.jobs.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.jobs.image.repository (default .Chart.AppVersion .Values.jobs.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.jobSidecarImage" -}}
{{- if .Values.jobs.sidecarImage.ref -}}
{{- .Values.jobs.sidecarImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.jobs.sidecarImage.repository (default .Chart.AppVersion .Values.jobs.sidecarImage.tag) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.jobNamespace" -}}
{{- default .Release.Namespace .Values.orchestrator.jobNamespace -}}
{{- end -}}

{{- define "orchestrator.artifactEndpoint" -}}
{{- if .Values.orchestrator.artifactEndpoint -}}
{{- .Values.orchestrator.artifactEndpoint -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%d" (include "orchestrator.jobsName" .) .Release.Namespace (int .Values.service.apiPort) -}}
{{- end -}}
{{- end -}}

{{/*
S3 credential env for a service. Call with a dict: {s3: <s3 values>, secretName: <secret>}.
Renders nothing when s3.enabled is false. Endpoint/region/path-style are plain
values; the keys come from a Secret (secretKeyRef) so they never rest in the
pod manifest.
*/}}
{{- define "orchestrator.s3Env" -}}
{{- $s3 := .s3 -}}
{{- if $s3.enabled -}}
{{- if $s3.endpoint }}
- name: S3_ENDPOINT
  value: {{ $s3.endpoint | quote }}
{{- end }}
- name: S3_REGION
  value: {{ $s3.region | quote }}
{{- if $s3.forcePathStyle }}
- name: S3_FORCE_PATH_STYLE
  value: "true"
{{- end }}
- name: S3_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .secretName }}
      key: S3_ACCESS_KEY_ID
- name: S3_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .secretName }}
      key: S3_SECRET_ACCESS_KEY
{{- end -}}
{{- end -}}

{{/* Resolve the S3 secret name for a service: an existing secret, or the chart-created one. */}}
{{- define "orchestrator.jobsS3SecretName" -}}
{{- default (printf "%s-s3" (include "orchestrator.jobsName" .)) .Values.jobs.s3.existingSecret -}}
{{- end -}}

{{- define "orchestrator.deploymentsS3SecretName" -}}
{{- default (printf "%s-s3" (include "orchestrator.deploymentsName" .)) .Values.deployments.s3.existingSecret -}}
{{- end -}}

{{- define "orchestrator.leaseName" -}}
{{- if .Values.leaderElection.leaseName -}}
{{- .Values.leaderElection.leaseName -}}
{{- else -}}
{{- printf "%s-leader" (include "orchestrator.jobsName" .) -}}
{{- end -}}
{{- end -}}

{{/*
  labels: the recommended label set, for resource metadata only. Deliberately
  NOT the selector — version changes on every release and selectors are
  immutable, which is why every component keeps a separate SelectorLabels
  define.
*/}}
{{- define "orchestrator.labels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | replace "+" "_" | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
  podAnnotations: a component's user annotations, plus a checksum of the
  chart-managed S3 credentials so that `helm upgrade` with rotated keys
  actually rolls the pods — without it the old credentials stay live in the
  running ones. Call with {annotations, s3}; renders nothing when there is
  neither. An existingSecret is not hashed: its contents are not ours to see,
  so rotating it is the operator's rollout to trigger.

  Merged into one dict rather than emitted as two blocks, so a user
  annotation named checksum/s3 cannot shadow the generated one — as a
  duplicate YAML key it would win and freeze the marker, quietly defeating
  the rollout it exists to trigger. deepCopy because .Values is shared across
  templates and set would otherwise mutate it.
*/}}
{{- define "orchestrator.podAnnotations" -}}
{{- $annotations := deepCopy (default (dict) .annotations) -}}
{{- if and .s3.enabled (not .s3.existingSecret) -}}
{{- $_ := set $annotations "checksum/s3" (.s3 | toJson | sha256sum) -}}
{{- end -}}
{{- with $annotations -}}
annotations:
{{- toYaml . | nindent 2 }}
{{- end -}}
{{- end -}}

{{/*
  jobsLabels: labels specific to the jobs API resources; used both on the
  Deployment/Service metadata and as the selector so they match.
*/}}
{{- define "orchestrator.jobsLabels" -}}
{{ include "orchestrator.labels" . }}
app.kubernetes.io/component: jobs
{{- end -}}

{{- define "orchestrator.jobsSelectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: jobs
{{- end -}}

{{/*
Hardened pod-level security context for control-plane pods — the same floor
the orchestrator stamps on workload pods.
*/}}
{{- define "orchestrator.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{/*
Hardened container-level security context for control-plane containers.
*/}}
{{- define "orchestrator.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end }}

{{/*
Zero-downtime rollout strategy: surge a replacement before taking a replica
away. Safe for leader-elected components — surged replicas are followers.
*/}}
{{- define "orchestrator.rolloutStrategy" -}}
type: RollingUpdate
rollingUpdate:
  maxUnavailable: 0
  maxSurge: 1
{{- end }}

{{- define "orchestrator.sandboxEdgeLabels" -}}
{{ include "orchestrator.labels" . }}
app.kubernetes.io/component: sandbox-edge
{{- end -}}

{{- define "orchestrator.sandboxEdgeSelectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: sandbox-edge
{{- end -}}
