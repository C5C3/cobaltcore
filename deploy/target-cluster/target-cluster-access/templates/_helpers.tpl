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

{{/*
Refuse a values.privilegedNamespaces the chart cannot honour: an entry that is
not in values.namespaces, and any entry at all while createNamespaces is false.

The label lands on a Namespace this chart writes, so in both cases it lands
nowhere while the install still reports success. The first sign of it is a
chassis DaemonSet whose pods PodSecurity admission rejects, on a cluster that
enforces baseline or restricted.

The opposite direction has no guard, because the chart cannot see it. With
createNamespaces false it never writes the Namespace, so it neither sets the
label nor clears one already there — an empty privilegedNamespaces then means
only that nothing is being asked for, not that PodSecurity is enforcing. See
"REMOVING IT AGAIN" in values.yaml for how to read the live posture instead.
*/}}
{{- define "target-cluster-access.requirePrivilegedNamespaces" -}}
{{- range $entry := .Values.privilegedNamespaces }}
{{- if not (has $entry $.Values.namespaces) }}
{{- fail (printf "values.privilegedNamespaces entry %q is not in values.namespaces" $entry) }}
{{- end }}
{{- end }}
{{- if and .Values.privilegedNamespaces (not .Values.createNamespaces) }}
{{- fail "values.privilegedNamespaces requires createNamespaces: true (the chart cannot label a namespace it does not create)" }}
{{- end }}
{{- end }}
