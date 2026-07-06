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
{{- printf "%s:%s" .Values.deployments.image.repository .Values.deployments.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.deploymentsSidecarImage" -}}
{{- if .Values.deployments.sidecarImage.ref -}}
{{- .Values.deployments.sidecarImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.sidecarImage.repository .Values.deployments.sidecarImage.tag -}}
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
{{- printf "%s:%s" .Values.deployments.shimImage.repository .Values.deployments.shimImage.tag -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.activatorImage" -}}
{{- if .Values.deployments.activator.image.ref -}}
{{- .Values.deployments.activator.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.deployments.activator.image.repository .Values.deployments.activator.image.tag -}}
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

{{- define "orchestrator.jobsImage" -}}
{{- if .Values.jobs.image.ref -}}
{{- .Values.jobs.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.jobs.image.repository .Values.jobs.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.jobSidecarImage" -}}
{{- if .Values.jobs.sidecarImage.ref -}}
{{- .Values.jobs.sidecarImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.jobs.sidecarImage.repository .Values.jobs.sidecarImage.tag -}}
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

{{- define "orchestrator.leaseName" -}}
{{- if .Values.leaderElection.leaseName -}}
{{- .Values.leaderElection.leaseName -}}
{{- else -}}
{{- printf "%s-leader" (include "orchestrator.jobsName" .) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.labels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
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
