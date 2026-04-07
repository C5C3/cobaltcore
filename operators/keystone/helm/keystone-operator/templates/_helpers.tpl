{{/*
Expand the name of the chart.
*/}}
{{- define "keystone-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "keystone-operator.fullname" -}}
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
{{- define "keystone-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "keystone-operator.labels" -}}
helm.sh/chart: {{ include "keystone-operator.chart" . }}
{{ include "keystone-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "keystone-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "keystone-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "keystone-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "keystone-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role (CC-0043).
Extracted into a named template to prevent drift when rules change.
*/}}
{{- define "keystone-operator.rbacRules" -}}
# keystone.openstack.c5c3.io - keystones (CC-0017)
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# keystone.openstack.c5c3.io - keystones/status (CC-0017)
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones/status
  verbs:
    - get
    - update
    - patch
# keystone.openstack.c5c3.io - keystones/finalizers (CC-0017)
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones/finalizers
  verbs:
    - update
# apps - deployments (CC-0017)
- apiGroups:
    - apps
  resources:
    - deployments
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - services, configmaps, secrets, serviceaccounts (CC-0017)
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
    - secrets
    - serviceaccounts
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - events (CC-0017)
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
# batch - jobs, cronjobs (CC-0017)
- apiGroups:
    - batch
  resources:
    - jobs
    - cronjobs
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# k8s.mariadb.com - databases, users, grants (CC-0017)
- apiGroups:
    - k8s.mariadb.com
  resources:
    - databases
    - users
    - grants
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - externalsecrets, pushsecrets (CC-0017)
- apiGroups:
    - external-secrets.io
  resources:
    - externalsecrets
    - pushsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
# rbac.authorization.k8s.io - roles, rolebindings (CC-0017)
- apiGroups:
    - rbac.authorization.k8s.io
  resources:
    - roles
    - rolebindings
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# policy - poddisruptionbudgets (CC-0037)
- apiGroups:
    - policy
  resources:
    - poddisruptionbudgets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# autoscaling - horizontalpodautoscalers (CC-0038)
- apiGroups:
    - autoscaling
  resources:
    - horizontalpodautoscalers
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# networking.k8s.io - networkpolicies (CC-0039)
- apiGroups:
    - networking.k8s.io
  resources:
    - networkpolicies
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# coordination.k8s.io - leases for leader election (CC-0018)
- apiGroups:
    - coordination.k8s.io
  resources:
    - leases
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
{{- end }}
