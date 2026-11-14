{{/* 通用模板：名称、标签、镜像 */}}

{{- define "godw.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "godw.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "godw.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "godw.labels" -}}
app.kubernetes.io/name: {{ include "godw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "godw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "godw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "godw.image" -}}
{{ printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/* 数据库密码来源：外部 Secret 优先，其次内置 PG */}}
{{- define "godw.dbSecretName" -}}
{{- if .Values.postgresql.enabled -}}
{{ include "godw.fullname" . }}-postgresql
{{- else -}}
{{ required "externalDatabase.existingSecret 必填" .Values.externalDatabase.existingSecret }}
{{- end -}}
{{- end }}

{{- define "godw.jwtSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{ .Values.auth.existingSecret }}
{{- else -}}
{{ include "godw.fullname" . }}-auth
{{- end -}}
{{- end }}
