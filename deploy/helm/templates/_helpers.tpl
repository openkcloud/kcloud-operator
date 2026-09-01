{{/*
Expand the name of the chart.
*/}}
{{- define "kcloud-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kcloud-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name "controller-manager" | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "kcloud-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{ .Values.serviceAccount.name }}
{{- else -}}
{{ include "kcloud-operator.fullname" . }}
{{- end -}}
{{- end -}}

{{/*
Common selector labels
*/}}
{{- define "kcloud-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kcloud-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Assemble a fully-qualified image string from a registry-relative repo + tag.
Input dict keys: registry (may be empty), repo, tag.
- registry 가 비어있지 않으면 "<registry>/<repo>:<tag>" 로 prefix.
- 비어있으면 "<repo>:<tag>" (registry-relative, e.g. Docker Hub 기본).
사설 IP 를 values 기본값에서 제거하고 install 시 global.registry 로 1줄 주입하기 위함.
*/}}
{{- define "kcloud-operator.image" -}}
{{- $reg := .registry | default "" -}}
{{- if $reg -}}{{ $reg }}/{{ .repo }}:{{ .tag }}{{- else -}}{{ .repo }}:{{ .tag }}{{- end -}}
{{- end -}}

{{/*
Assemble a 3rd-party(vendor) image string.
Input dict keys: registry (global.vendorRegistry, may be empty), upstream (vendor host,
e.g. "docker.io"), repo ("<org>/<image>"), tag.
- registry 가 비어 있으면 "<upstream>/<repo>:<tag>" — 벤더 공개 레지스트리에서 직접 당긴다.
- registry 가 채워져 있으면 "<registry>/<repo>:<tag>" — air-gap 사설 미러 경로다.
미러 경로 규약은 upstream 의 org 경로를 그대로 유지하는 것이다(<registry>/<org>/<image>).
kcloud 가 빌드하는 이미지는 이 헬퍼가 아니라 kcloud-operator.image + global.registry 를 쓴다.
*/}}
{{- define "kcloud-operator.vendorImage" -}}
{{- $reg := .registry | default .upstream | default "" -}}
{{- include "kcloud-operator.image" (dict "registry" $reg "repo" .repo "tag" .tag) -}}
{{- end -}}
