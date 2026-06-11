{{/*
Shared naming, label, and service-account helpers for the operator charts.

These templates are defined once here and consumed by both operator charts so a
change lands in one place. Every helper resolves against the CONSUMING chart's
context (.Chart, .Release, .Values) — when a parent template calls these with the
root context, .Chart.Name yields the parent operator chart name (e.g.
"keystone-operator"), not "operator-library".
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "operator-library.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "operator-library.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "operator-library.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "operator-library.labels" -}}
helm.sh/chart: {{ include "operator-library.chart" . }}
{{ include "operator-library.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "operator-library.selectorLabels" -}}
app.kubernetes.io/name: {{ include "operator-library.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "operator-library.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "operator-library.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
