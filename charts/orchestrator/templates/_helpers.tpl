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

{{- define "orchestrator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else -}}
{{- include "orchestrator.fullname" . -}}
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

{{- define "orchestrator.artifactEndpoint" -}}
{{- if .Values.orchestrator.artifactEndpoint -}}
{{- .Values.orchestrator.artifactEndpoint -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%d" (include "orchestrator.fullname" .) .Release.Namespace (int .Values.service.apiPort) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.leaseName" -}}
{{- if .Values.leaderElection.leaseName -}}
{{- .Values.leaderElection.leaseName -}}
{{- else -}}
{{- printf "%s-leader" (include "orchestrator.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "orchestrator.labels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "orchestrator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
