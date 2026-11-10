{{/*
Argus · mock-llm 公共模板
命名规则：release 名已含 "mock-llm" 时不重复拼接（避免 mock-llm-mock-llm）。
*/}}

{{- define "mock-llm.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "mock-llm.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "mock-llm.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mock-llm.labels" -}}
helm.sh/chart: {{ include "mock-llm.chart" . }}
{{ include "mock-llm.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: argus-mock
{{- end -}}

{{- define "mock-llm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mock-llm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
