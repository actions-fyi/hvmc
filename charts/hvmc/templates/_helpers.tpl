{{/*
Object names are fixed (DESIGN §4.6, §6, §11): the coordinator finds its
Services, Secrets and ConfigMaps by these names, and the chart is installed
once per cluster.
*/}}

{{/* Labels shared by every object of the chart. */}}
{{- define "hvmc.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: hvmc
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: hvmc
{{- end }}

{{/* Selector labels of the coordinator pods. */}}
{{- define "hvmc.coordinator.selectorLabels" -}}
app.kubernetes.io/name: hvmc
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: coordinator
{{- end }}

{{/* Labels of coordinator objects. */}}
{{- define "hvmc.coordinator.labels" -}}
{{ include "hvmc.labels" . }}
app.kubernetes.io/component: coordinator
{{- end }}

{{/*
Label the leader sets on its own pod once it holds the lease and has rebuilt
the ledger (DESIGN §6). Leader-only Services select it.
*/}}
{{- define "hvmc.leaderLabel" -}}
hvmc.fax.fyi/leader: "true"
{{- end }}

{{/* Coordinator image reference; a digest wins over the tag. */}}
{{- define "hvmc.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end }}

{{/* hvmc version of the agents NOTES.txt tells operators to install. */}}
{{- define "hvmc.version" -}}
{{ .Values.image.tag | default .Chart.AppVersion | trimPrefix "v" }}
{{- end }}

{{/* Coordinator container ports (DESIGN §8: facade and enrollment, metrics). */}}
{{- define "hvmc.port.http" -}}8080{{- end }}
{{- define "hvmc.port.metrics" -}}9090{{- end }}
{{- define "hvmc.port.health" -}}8081{{- end }}

{{/* Default agent gRPC port (DESIGN §4.7). */}}
{{- define "hvmc.port.agent" -}}9443{{- end }}

{{/*
Agent gRPC ports the coordinator dials: the default, every port declared in
hostConfig and networkPolicy.agentPorts. Rendered as a sorted JSON list.
*/}}
{{- define "hvmc.agentPorts" -}}
{{- $ports := list (include "hvmc.port.agent" . | int) -}}
{{- if not .Values.hostConfig.existing -}}
{{- range .Values.hostConfig.agents -}}
{{- $ports = append $ports (.port | default 0 | int) -}}
{{- end -}}
{{- range .Values.hostConfig.vms -}}
{{- $ports = append $ports (dig "guestAgent" "port" 0 . | int) -}}
{{- end -}}
{{- end -}}
{{- range .Values.networkPolicy.agentPorts -}}
{{- $ports = append $ports (. | int) -}}
{{- end -}}
{{- without $ports 0 | uniq | sortAlpha | toJson -}}
{{- end }}

{{/*
Enrollment base URL for NOTES.txt, or "" when it is only known after the
load balancer or Ingress got an address.
*/}}
{{- define "hvmc.enrollURL" -}}
{{- $ing := .Values.facade.ingress -}}
{{- if and $ing.enabled $ing.host -}}
{{ ternary "https" "http" (not (empty $ing.tls)) }}://{{ $ing.host }}/v1/enroll
{{- end -}}
{{- end }}
