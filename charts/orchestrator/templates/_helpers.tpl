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

{{- define "orchestrator.pauseImage" -}}
{{- if .Values.preload.pauseImage.ref -}}
{{- .Values.preload.pauseImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.preload.pauseImage.repository .Values.preload.pauseImage.tag -}}
{{- end -}}
{{- end -}}

{{/*
  preloadSelectorLabels: identity of a plane's pre-pull DaemonSet. Takes a
  dict {root, component} rather than the root context, because one define
  serves both planes.
*/}}
{{- define "orchestrator.preloadSelectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}-preload
{{- end -}}

{{/*
  preloadDaemonSet: a DaemonSet that pre-pulls a plane's images onto the nodes
  that plane's workloads land on, so the first job/revision scheduled to a
  fresh node doesn't pay the pull. Call with a dict:
  {root, name, component, images, nodeSelector, tolerations}.

  Each image is pulled by giving it a container — but a pre-pull container has
  to exit 0, and our images are distroless with no shell, so there is nothing
  generic to run inside them. The pool-shim already solves exactly this for
  warm pods: `-install <path>` copies its own static binary out and exits. So
  one init container installs the shim into a shared emptyDir and every
  pre-pull container execs that binary, each writing its copy to its own path
  (the images run as different users, so they cannot overwrite each other's).

  Deliberately no runAsNonRoot in the pod security context — extraImages are
  arbitrary user runtimes and some of them are root images. fsGroup is what
  lets them write into the emptyDir regardless of their user.
*/}}
{{- define "orchestrator.preloadDaemonSet" -}}
{{- $root := .root -}}
{{- $labels := dict "root" $root "component" .component -}}
{{- $images := list -}}
{{- range .images }}{{- if and . (not (has . $images)) }}{{- $images = append $images . }}{{- end }}{{- end }}
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{ .name }}-preload
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- include "orchestrator.labels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ .component }}-preload
spec:
  selector:
    matchLabels:
      {{- include "orchestrator.preloadSelectorLabels" $labels | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "orchestrator.preloadSelectorLabels" $labels | nindent 8 }}
    spec:
      # A private registry can be reached two ways: chart-level
      # imagePullSecrets, or a ServiceAccount that already carries them (the
      # route workload pods use). Both work here — the ServiceAccount
      # admission plugin merges an SA's pull secrets into the pod regardless
      # of token automount, which stays off since nothing here calls the API.
      {{- with $root.Values.preload.serviceAccountName }}
      serviceAccountName: {{ . | quote }}
      {{- end }}
      automountServiceAccountToken: false
      {{- with include "orchestrator.imagePullSecrets" $root }}
      {{- . | nindent 6 }}
      {{- end }}
      # Pre-pulling is never worth evicting or outbidding real work.
      priorityClassName: {{ $root.Values.preload.priorityClassName | quote }}
      {{- with .nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        seccompProfile:
          type: RuntimeDefault
        # Makes the shared emptyDir group-writable for every image's own user.
        fsGroup: 65532
      volumes:
        - name: shim
          emptyDir: {}
      initContainers:
        - name: install-shim
          image: {{ include "orchestrator.poolShimImage" $root | quote }}
          imagePullPolicy: IfNotPresent
          command: ["/ko-app/pool-shim", "-install", "/shim/noop"]
          securityContext:
            {{- include "orchestrator.containerSecurityContext" $root | nindent 12 }}
          resources:
            {{- include "orchestrator.preloadResources" $root | nindent 12 }}
          volumeMounts:
            - name: shim
              mountPath: /shim
        {{- range $i, $image := $images }}
        - name: preload-{{ $i }}
          image: {{ $image | quote }}
          imagePullPolicy: IfNotPresent
          command: ["/shim/noop", "-install", "/shim/noop-{{ $i }}"]
          securityContext:
            {{- include "orchestrator.containerSecurityContext" $root | nindent 12 }}
          resources:
            {{- include "orchestrator.preloadResources" $root | nindent 12 }}
          volumeMounts:
            - name: shim
              mountPath: /shim
        {{- end }}
      containers:
        # Once the pulls are done the pod only has to stay alive, so that the
        # images it pulled stay pinned against kubelet's image GC. pause is
        # the upstream image for exactly that and does nothing else.
        - name: pause
          image: {{ include "orchestrator.pauseImage" $root | quote }}
          imagePullPolicy: IfNotPresent
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            {{- include "orchestrator.containerSecurityContext" $root | nindent 12 }}
          resources:
            {{- include "orchestrator.preloadResources" $root | nindent 12 }}
{{- end -}}

{{/* Pre-pull containers do nothing but exist; keep them out of the scheduler's way. */}}
{{- define "orchestrator.preloadResources" -}}
requests:
  cpu: 1m
  memory: 8Mi
limits:
  memory: 32Mi
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
