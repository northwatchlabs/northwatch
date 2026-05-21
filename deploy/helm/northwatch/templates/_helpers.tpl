{{/*
Common helpers for the northwatch chart.
*/}}

{{/*
Chart "fullname" — used as the resource name prefix.
Truncated to 63 chars and trailing dashes stripped so it stays valid
across all kinds (Deployment, Service, ClusterRole, ...).
*/}}
{{- define "northwatch.fullname" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "northwatch.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "northwatch.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard recommended labels.
*/}}
{{- define "northwatch.labels" -}}
helm.sh/chart: {{ include "northwatch.chart" . }}
{{ include "northwatch.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "northwatch.selectorLabels" -}}
app.kubernetes.io/name: {{ include "northwatch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name — explicit override or derived from fullname.
*/}}
{{- define "northwatch.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "northwatch.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the API token (existing or chart-managed).
*/}}
{{- define "northwatch.tokenSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-api-token" (include "northwatch.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Key inside the token Secret. existingSecretKey wins when existingSecret
is set; chart-managed Secret always uses key "token".
*/}}
{{- define "northwatch.tokenSecretKey" -}}
{{- if .Values.auth.existingSecret -}}
{{- default "token" .Values.auth.existingSecretKey -}}
{{- else -}}
token
{{- end -}}
{{- end -}}
