{{/*
Expand the name of the chart.
*/}}
{{- define "c5c3-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "c5c3-operator.fullname" -}}
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
{{- define "c5c3-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "c5c3-operator.labels" -}}
helm.sh/chart: {{ include "c5c3-operator.chart" . }}
{{ include "c5c3-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "c5c3-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "c5c3-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "c5c3-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "c5c3-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role (CC-0043).
Extracted into a named template to prevent drift when rules change.
Rules mirror the +kubebuilder:rbac markers on the ControlPlane and
CredentialRotation reconcilers (CC-0110), deduplicated across both, plus the
coordination.k8s.io/leases rule required for leader election.
*/}}
{{- define "c5c3-operator.rbacRules" -}}
# c5c3.io - controlplanes (CC-0110)
- apiGroups:
    - c5c3.io
  resources:
    - controlplanes
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# c5c3.io - controlplanes/status (CC-0110)
- apiGroups:
    - c5c3.io
  resources:
    - controlplanes/status
  verbs:
    - get
    - update
    - patch
# c5c3.io - controlplanes/finalizers (CC-0110)
- apiGroups:
    - c5c3.io
  resources:
    - controlplanes/finalizers
  verbs:
    - update
# c5c3.io - credentialrotations (CC-0110)
- apiGroups:
    - c5c3.io
  resources:
    - credentialrotations
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# c5c3.io - credentialrotations/status (CC-0110)
- apiGroups:
    - c5c3.io
  resources:
    - credentialrotations/status
  verbs:
    - get
    - update
    - patch
# c5c3.io - secretaggregates (read-only, CC-0110)
# The ControlPlane reconciler only observes SecretAggregate CRs; it never
# creates or mutates them, so the rule is intentionally read-only.
- apiGroups:
    - c5c3.io
  resources:
    - secretaggregates
  verbs:
    - get
    - list
    - watch
# k8s.mariadb.com - mariadbs (CC-0110)
# Projected and Owned by reconcileInfrastructure.
- apiGroups:
    - k8s.mariadb.com
  resources:
    - mariadbs
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# memcached.c5c3.io - memcacheds (CC-0110)
# Projected and Owned by reconcileInfrastructure (resolved via the cluster
# RESTMapper at runtime; no Go scheme registration required).
- apiGroups:
    - memcached.c5c3.io
  resources:
    - memcacheds
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# keystone.openstack.c5c3.io - keystones (CC-0110)
# The ControlPlane reconciler projects and Owns a Keystone child.
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
# openstack.k-orc.cloud - applicationcredentials, services, endpoints (CC-0110)
# Minted and Owned by reconcileKORC and reconcileCatalog.
- apiGroups:
    - openstack.k-orc.cloud
  resources:
    - applicationcredentials
    - services
    - endpoints
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - externalsecrets, pushsecrets (CC-0110)
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
    - delete
# external-secrets.io - clustersecretstores (read-only, CC-0110)
# Required so the operator can observe the ClusterSecretStore's Ready
# condition and reflect upstream secret-backend outages. ClusterSecretStore is
# cluster-scoped; the rule is only effective when rendered into the ClusterRole.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
  verbs:
    - get
    - list
    - watch
# core - secrets (CC-0110)
- apiGroups:
    - ""
  resources:
    - secrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - events (CC-0110)
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
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
