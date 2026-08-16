{{/*
Naming and label helpers. They mirror the operator charts' operator-library
helpers, minus the override values this chart does not carry: the objects it
installs are addressed by the cluster owner from the values, not overridden.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "target-cluster-access.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name. It names the Role and RoleBinding in
every declared namespace and the cluster-scoped pair, so a second install on the
same cluster does not meet the first one's objects.
*/}}
{{- define "target-cluster-access.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "target-cluster-access.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "target-cluster-access.labels" -}}
helm.sh/chart: {{ include "target-cluster-access.chart" . }}
{{ include "target-cluster-access.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "target-cluster-access.selectorLabels" -}}
app.kubernetes.io/name: {{ include "target-cluster-access.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Refuse an install that declares no namespace. Every template includes this, so
the refusal reaches whichever one Helm renders first.

An empty list is not a smaller install, it is a broken one: the account would
authenticate and then be forbidden everywhere, and the first sign of it is a CR
on the management cluster parked on a forbidden message. The namespaces are also
the only input the chart cannot derive, because they have to match a Secret on
the other cluster.
*/}}
{{- define "target-cluster-access.requireNamespaces" -}}
{{- if not .Values.namespaces }}
{{- fail "values.namespaces must name at least one namespace (the namespaces key of the registration Secret)" }}
{{- end }}
{{- end }}
