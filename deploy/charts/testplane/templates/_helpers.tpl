{{/*
标签与命名。约定：
  - name      = Chart.Name（testplane），可用 nameOverride 覆盖
  - fullname  = <release>-<name>，除非 release 名已包含 chart 名
  - selectorLabels 一旦发布**不能改**（Deployment selector 不可变）
*/}}
{{- define "testplane.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "testplane.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "testplane.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "testplane.labels" -}}
helm.sh/chart: {{ include "testplane.chart" . }}
{{ include "testplane.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: testplane
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "testplane.selectorLabels" -}}
app.kubernetes.io/name: {{ include "testplane.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "testplane.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "testplane.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
镜像引用：digest 优先（W-8「tag 与 digest 二选一」；release 环境强制 digest）。
两者都没给时直接 fail，避免渲染出一个 `repo:` 的空标签。
*/}}
{{- define "testplane.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else if .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- else }}
{{- fail "image.tag 或 image.digest 必须给一个（生产请用 digest）" }}
{{- end }}
{{- end }}

{{/*
产物 PVC 名：Tester Job 渲染时也必须写这个名（否则 invalid job spec: storage PVC is required）。
persistence.existingClaim 优先，其次本 chart 创建的 PVC 名。
*/}}
{{- define "testplane.storageClaim" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- printf "%s-storage" (include "testplane.fullname" .) }}
{{- end }}
{{- end }}

{{- define "testplane.executorNamespace" -}}
{{- default .Release.Namespace .Values.config.executorNamespace }}
{{- end }}

{{/*
必填项校验（渲染期就失败，而不是等 Pod CrashLoop）。
*/}}
{{- define "testplane.validate" -}}
{{- if eq .Values.config.authMode "oidc" }}
{{- if not .Values.oidc.issuer }}
{{- fail "config.authMode=oidc 必须给 oidc.issuer（统一身份源地址，D-51）" }}
{{- end }}
{{- if not .Values.oidc.clientID }}
{{- fail "config.authMode=oidc 必须给 oidc.clientID（TestPlane 独立 client，D-51）" }}
{{- end }}
{{- if not .Values.oidc.clientSecretRef.name }}
{{- fail "config.authMode=oidc 必须给 oidc.clientSecretRef.name（client secret 只走 Secret）" }}
{{- end }}
{{- if not .Values.session.secretRef.name }}
{{- fail "config.authMode=oidc 必须给 session.secretRef.name（会话 HMAC 密钥 ≥32 字节，只走 Secret）" }}
{{- end }}
{{- else if eq .Values.config.authMode "dev" }}
{{- if not (has .Values.config.env (list "dev" "local")) }}
{{- fail "config.authMode=dev 只能与 config.env=dev|local 一起用（dev 模式下任何人都是 admin）" }}
{{- end }}
{{- else }}
{{- fail "config.authMode 只能是 oidc|dev" }}
{{- end }}
{{- if eq .Values.config.deployProvider "genet" }}
{{- if not .Values.genet.baseURL }}
{{- fail "config.deployProvider=genet 必须给 genet.baseURL（Genet Open API 地址）" }}
{{- end }}
{{- if not .Values.genet.apiKeySecretRef.name }}
{{- fail "config.deployProvider=genet 必须给 genet.apiKeySecretRef.name（Bearer key 只走 Secret）" }}
{{- end }}
{{- end }}
{{- if and (not .Values.database.existingSecret) (not .Values.database.dsn) }}
{{- fail "database.existingSecret 必须给（DB DSN 含口令，不允许明文进 ConfigMap）" }}
{{- end }}
{{- if and .Values.database.existingSecret .Values.database.dsn }}
{{- fail "database.existingSecret 与 database.dsn 只能选一个" }}
{{- end }}
{{- end }}
