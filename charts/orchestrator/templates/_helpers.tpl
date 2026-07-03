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

{{- define "orchestrator.image" -}}
{{- if .Values.image.ref -}}
{{- .Values.image.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.sidecarImage" -}}
{{- if .Values.sidecarImage.ref -}}
{{- .Values.sidecarImage.ref -}}
{{- else -}}
{{- printf "%s:%s" .Values.sidecarImage.repository .Values.sidecarImage.tag -}}
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
