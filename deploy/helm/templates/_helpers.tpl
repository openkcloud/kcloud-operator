{{/*
Expand the name of the chart.
*/}}
{{- define "npu-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "npu-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name "controller-manager" | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "npu-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{ .Values.serviceAccount.name }}
{{- else -}}
{{ include "npu-operator.fullname" . }}
{{- end -}}
{{- end -}}

{{/*
Common selector labels
*/}}
{{- define "npu-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "npu-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
