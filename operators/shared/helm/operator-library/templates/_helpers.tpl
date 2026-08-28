{{/*
Shared naming, label, and service-account helpers for the operator charts.

These templates are defined once here and consumed by every operator chart so a
change lands in one place. Every helper resolves against the CONSUMING chart's
context (.Chart, .Release, .Values) — when a parent template calls these with the
root context, .Chart.Name yields the parent operator chart name (e.g.
"keystone-operator"), not "operator-library".
*/}}

{{/*
Chart hooks.

The "operator-library.chart.*" templates are the points where an operator
chart injects what only it knows, without the library naming any operator.
The library defines every hook empty; a chart overrides one by defining a
template of the same name in its own templates/_helpers.tpl. Helm loads the
parent chart's templates after the library's, so the chart's definition wins.

  operator-library.chart.namespaceScopedUnsupported
      Non-empty: the reason rbac.namespaceScoped=true cannot work for this
      operator. The Role template fails the render with it.
  operator-library.chart.networkPolicyEgress
      Extra egress rules (a YAML list, rendered at the egress list's indent)
      appended to the shared NetworkPolicy.
  operator-library.chart.args
      Extra manager container arguments (a YAML list) the chart derives from
      its own values, rendered before the shared --metrics-bind-address flag.
      Verbatim user-supplied flags go through .Values.extraArgs instead.
  operator-library.chart.env
      Environment variables (a YAML list of name/value entries) the chart
      derives from its own values. Verbatim user-supplied variables go through
      .Values.extraEnv instead.
*/}}
{{- define "operator-library.chart.namespaceScopedUnsupported" -}}
{{- end }}

{{- define "operator-library.chart.networkPolicyEgress" -}}
{{- end }}

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

{{- define "operator-library.chart.args" -}}
{{- end }}

{{- define "operator-library.chart.env" -}}
{{- end }}
