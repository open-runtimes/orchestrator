{{- define "orchestrator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
  fullname: the prefix every resource name is built from, by the usual Helm
  convention: the release name when it already contains the chart name,
  otherwise "<release>-<chart>".
*/}}
{{- define "orchestrator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "orchestrator.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* componentName: "<fullname>-<component>". Call with (dict "root" $ "component" "jobs"). */}}
{{- define "orchestrator.componentName" -}}
{{- printf "%s-%s" (include "orchestrator.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
  labels: the recommended label set, for metadata only. Not the selector:
  version changes every release and selectors are immutable.
*/}}
{{- define "orchestrator.labels" -}}
{{ include "orchestrator.selectorLabels" . }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | replace "+" "_" | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
helm.sh/chart: {{ .root.Chart.Name }}-{{ .root.Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "orchestrator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orchestrator.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- end -}}

{{/* image: ref, or repository:tag with the chart's appVersion as the default tag. */}}
{{- define "orchestrator.image" -}}
{{- if .image.ref -}}
{{- .image.ref -}}
{{- else -}}
{{- printf "%s:%s" .image.repository (default .root.Chart.AppVersion .image.tag) -}}
{{- end -}}
{{- end -}}

{{/* planeNamespace: where a plane's workload pods run. Call with (dict "root" $ "plane" "jobs"). */}}
{{- define "orchestrator.planeNamespace" -}}
{{- default .root.Release.Namespace (index .root.Values .plane "namespace") -}}
{{- end -}}

{{/* serviceAccountName: a component's account. Call with (dict "root" $ "component" "jobs" "values" .Values.jobs). */}}
{{- define "orchestrator.serviceAccountName" -}}
{{- default (include "orchestrator.componentName" .) .values.serviceAccount.name -}}
{{- end -}}

{{- define "orchestrator.jobSidecarServiceAccountName" -}}
{{- default (include "orchestrator.componentName" (dict "root" . "component" "job-sidecar")) .Values.jobs.workload.serviceAccount.name -}}
{{- end -}}

{{- define "orchestrator.s3SecretName" -}}
{{- default (printf "%s-s3" (include "orchestrator.componentName" (dict "root" .root "component" .plane))) (index .root.Values .plane "s3" "existingSecret") -}}
{{- end -}}

{{- define "orchestrator.gatewayName" -}}
{{- default (include "orchestrator.fullname" .) .Values.gateway.name -}}
{{- end -}}

{{- define "orchestrator.gatewayNamespace" -}}
{{- default .Release.Namespace .Values.gateway.namespace -}}
{{- end -}}

{{/*
  s3Env: a plane's S3 credentials: the default profile as S3_*, each named
  profile as S3_<NAME>_*, listed in S3_PROFILES. Endpoint, region and path
  style are plain values; the keys come from a Secret so they never rest in the
  pod manifest. Call with (dict "root" $ "plane" "jobs").
*/}}
{{- define "orchestrator.s3Env" -}}
{{- $s3 := index .root.Values .plane "s3" -}}
{{- if $s3.enabled -}}
{{- $secret := include "orchestrator.s3SecretName" . -}}
{{- include "orchestrator.s3ProfileEnv" (dict "prefix" "S3_" "p" $s3 "secret" $secret) }}
{{- with $s3.profiles }}
- name: S3_PROFILES
  value: {{ keys . | sortAlpha | join "," | quote }}
{{- range $name, $p := . }}
{{- include "orchestrator.s3ProfileEnv" (dict "prefix" (printf "S3_%s_" (upper $name)) "p" $p "secret" $secret) }}
{{- end }}
{{- end }}
{{- end -}}
{{- end -}}

{{- define "orchestrator.s3ProfileEnv" -}}
{{- if .p.endpoint }}
- name: {{ .prefix }}ENDPOINT
  value: {{ .p.endpoint | quote }}
{{- end }}
- name: {{ .prefix }}REGION
  value: {{ default "us-east-1" .p.region | quote }}
{{- if .p.forcePathStyle }}
- name: {{ .prefix }}FORCE_PATH_STYLE
  value: "true"
{{- end }}
{{- range $key := list "ACCESS_KEY_ID" "SECRET_ACCESS_KEY" }}
- name: {{ $.prefix }}{{ $key }}
  valueFrom:
    secretKeyRef:
      name: {{ $.secret }}
      key: {{ $.prefix }}{{ $key }}
{{- end }}
{{- end -}}

{{/*
  workloadEnv: how a plane's service shapes the pods it creates: their
  namespace, placement, resource model, mount agent and pull policies. Call
  with (dict "root" $ "plane" "deployments").
*/}}
{{- define "orchestrator.workloadEnv" -}}
{{- $root := .root -}}
{{- $workload := index $root.Values .plane "workload" }}
- name: KUBE_NAMESPACE
  value: {{ include "orchestrator.planeNamespace" . | quote }}
- name: KUBE_CPU_OVERCOMMIT
  value: {{ $workload.cpuOvercommit | quote }}
- name: KUBE_MEMORY_OVERCOMMIT
  value: {{ $workload.memoryOvercommit | quote }}
{{- with $workload.tolerations }}
- name: KUBE_WORKLOAD_TOLERATIONS
  value: {{ toJson . | quote }}
{{- end }}
{{- with $workload.nodeSelector }}
- name: KUBE_WORKLOAD_NODE_SELECTOR
  value: {{ toJson . | quote }}
{{- end }}
{{- if $root.Values.mountAgent.enabled }}
- name: KUBE_MOUNT_AGENT_PORT
  value: {{ $root.Values.mountAgent.port | quote }}
{{- end }}
{{- with $root.Values.workloadImagePullPolicy }}
- name: KUBE_WORKER_IMAGE_PULL_POLICY
  value: {{ . | quote }}
{{- end }}
{{- with $root.Values.images.pullPolicy }}
- name: KUBE_SIDECAR_IMAGE_PULL_POLICY
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
  leaderEnv: leader election for a component. Call with (dict "values"
  <leaderElection> "lease" <default lease name> "prefix" "KUBE_").
*/}}
{{- define "orchestrator.leaderEnv" -}}
{{- if .values.enabled }}
- name: {{ .prefix }}LEADER_ELECTION
  value: "true"
- name: {{ .prefix }}LEADER_LEASE_NAME
  value: {{ .lease | quote }}
- name: {{ .prefix }}LEADER_IDENTITY
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: {{ .prefix }}LEADER_LEASE_DURATION
  value: {{ printf "%ds" (int .values.leaseDurationSeconds) | quote }}
- name: {{ .prefix }}LEADER_RENEW_DEADLINE
  value: {{ printf "%ds" (int .values.renewDeadlineSeconds) | quote }}
- name: {{ .prefix }}LEADER_RETRY_PERIOD
  value: {{ printf "%ds" (int .values.retryPeriodSeconds) | quote }}
{{- end }}
{{- end -}}

{{/*
  podAnnotations: a component's annotations, plus a checksum of the
  chart-managed S3 credentials so an upgrade with rotated keys rolls the pods.
  Merged into one dict so a user annotation named checksum/s3 cannot shadow the
  generated one. An existingSecret is not hashed: rotating it is the
  operator's rollout to trigger.
*/}}
{{- define "orchestrator.podAnnotations" -}}
{{- $annotations := deepCopy (default (dict) .annotations) -}}
{{- if and .s3 .s3.enabled (not .s3.existingSecret) -}}
{{- $_ := set $annotations "checksum/s3" (.s3 | toJson | sha256sum) -}}
{{- end -}}
{{- with $annotations -}}
annotations:
{{- toYaml . | nindent 2 }}
{{- end -}}
{{- end -}}

{{/*
  component: a control-plane Deployment with its Service, ServiceAccount, PDB
  and HPA, rendered the same way for every component. Call with a dict:
    root       $
    component  name suffix and component label, e.g. "jobs"
    values     the component's values (image, replicaCount, autoscaling,
               resources, podAnnotations, nodeSelector, tolerations, affinity,
               priorityClassName, serviceAccount, service)
    container  the container name
    ports      [{name, port}]: container ports; "service" is the one the
               Service exposes, the rest stay pod-local
    probe      {port, readiness, liveness}: httpGet paths
    env        the component's own env (YAML list as a string)
    auth       the plane's auth values, mounted when existingSecret is set
    s3         the S3 values whose rotation should roll the pods
    labels     extra pod labels, e.g. the pool kind
    leader     whether the component can be leader-elected (replicas > 1 and
               autoscaling then require leaderElection.enabled)
*/}}
{{- define "orchestrator.component" -}}
{{- $root := .root -}}
{{- $v := .values -}}
{{- $name := include "orchestrator.componentName" . -}}
{{- $sel := include "orchestrator.selectorLabels" . -}}
{{- $labels := include "orchestrator.labels" . -}}
{{- $autoscaling := $v.autoscaling | default dict -}}
{{- if and .leader (or (gt (int $v.replicaCount) 1) $autoscaling.enabled) (not $v.leaderElection.enabled) -}}
{{- fail (printf "%s: replicaCount > 1 and autoscaling require leaderElection.enabled=true" .component) -}}
{{- end }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- $labels | nindent 4 }}
    {{- with .labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  {{- if not $autoscaling.enabled }}
  replicas: {{ $v.replicaCount }}
  {{- end }}
  # Surge a replacement before taking a replica away; surged replicas of a
  # leader-elected component are followers.
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      {{- $sel | nindent 6 }}
      {{- with .labels }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
  template:
    metadata:
      labels:
        {{- $sel | nindent 8 }}
        {{- with .labels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with include "orchestrator.podAnnotations" (dict "annotations" $v.podAnnotations "s3" .s3) }}
      {{- . | nindent 6 }}
      {{- end }}
    spec:
      terminationGracePeriodSeconds: {{ $root.Values.terminationGracePeriodSeconds }}
      serviceAccountName: {{ include "orchestrator.serviceAccountName" . }}
      {{- with $root.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $v.priorityClassName }}
      priorityClassName: {{ . }}
      {{- end }}
      {{- with $v.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $v.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $v.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              {{- $sel | nindent 14 }}
      volumes:
        # Scratch space so readOnlyRootFilesystem never bites a stray temp write.
        - name: tmp
          emptyDir: {}
        {{- if and .auth .auth.existingSecret }}
        - name: auth
          secret:
            secretName: {{ .auth.existingSecret }}
        {{- end }}
      containers:
        - name: {{ .container }}
          image: {{ include "orchestrator.image" (dict "root" $root "image" $v.image) | quote }}
          imagePullPolicy: {{ $v.image.pullPolicy }}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            {{- if and .auth .auth.existingSecret }}
            - name: auth
              mountPath: /var/run/secrets/orchestrator
              readOnly: true
            {{- end }}
          ports:
            {{- range .ports }}
            - name: {{ .name }}
              containerPort: {{ .port }}
            {{- end }}
          env:
            {{- .env | nindent 12 }}
            - name: SHUTDOWN_DRAIN_WAIT
              value: "0s"
            {{- if and .auth .auth.existingSecret }}
            - name: API_KEY_FILE
              value: /var/run/secrets/orchestrator/{{ .auth.secretKey }}
            {{- end }}
            {{- with $root.Values.extraEnv }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          readinessProbe:
            httpGet:
              path: {{ .probe.readiness }}
              port: {{ .probe.port }}
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: {{ .probe.liveness }}
              port: {{ .probe.port }}
            initialDelaySeconds: 10
            periodSeconds: 10
          resources:
            {{- toYaml $v.resources | nindent 12 }}
{{- with $v.service }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  type: {{ .type }}
  selector:
    {{- $sel | nindent 4 }}
  ports:
    - name: service
      port: {{ .port }}
      targetPort: service
{{- end }}
{{- if $v.serviceAccount.create }}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "orchestrator.serviceAccountName" . }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- $labels | nindent 4 }}
{{- end }}
{{- /* Only when durably multi-replica: a PDB on a possible singleton blocks node drains. */}}
{{- if or (gt (int $v.replicaCount) 1) (and $autoscaling.enabled (gt (int $autoscaling.minReplicas) 1)) }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      {{- $sel | nindent 6 }}
{{- end }}
{{- if $autoscaling.enabled }}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ $name }}
  minReplicas: {{ $autoscaling.minReplicas }}
  maxReplicas: {{ $autoscaling.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ $autoscaling.targetCPUUtilizationPercentage }}
{{- end }}
{{- end -}}

{{/*
  rbac: a Role in a namespace and its binding to a component's account.
  Call with (dict "root" $ "component" "jobs" "values" <component values>
  "namespace" <ns> "name" <role name> "rules" <rules YAML>).
*/}}
{{- define "orchestrator.rbac" -}}
{{- if .root.Values.rbac.create }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ .name }}
  namespace: {{ .namespace }}
  labels:
    {{- include "orchestrator.labels" . | nindent 4 }}
rules:
  {{- .rules | nindent 2 }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ .name }}
  namespace: {{ .namespace }}
  labels:
    {{- include "orchestrator.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ .name }}
subjects:
  - kind: ServiceAccount
    name: {{ include "orchestrator.serviceAccountName" . }}
    namespace: {{ .root.Release.Namespace }}
{{- end }}
{{- end -}}

{{/*
  runtimeClassReader: the one cluster-scoped grant a plane needs: isolation
  tiers are validated against cluster-scoped RuntimeClasses before workloads
  are accepted. Call with (dict "root" $ "component" ... "values" ...).
*/}}
{{- define "orchestrator.runtimeClassReader" -}}
{{- if .root.Values.rbac.create }}
{{- $name := printf "%s-runtimeclasses" (include "orchestrator.componentName" .) }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ $name }}
  labels:
    {{- include "orchestrator.labels" . | nindent 4 }}
rules:
  - apiGroups: ["node.k8s.io"]
    resources: ["runtimeclasses"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ $name }}
  labels:
    {{- include "orchestrator.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ $name }}
subjects:
  - kind: ServiceAccount
    name: {{ include "orchestrator.serviceAccountName" . }}
    namespace: {{ .root.Release.Namespace }}
{{- end }}
{{- end -}}
