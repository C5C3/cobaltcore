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
# core - pods (read-only, CC-0058)
# Required for getValidationErrorMessage to list pods of failed policy
# validation Jobs and extract error details from terminated container state.
- apiGroups:
    - ""
  resources:
    - pods
  verbs:
    - get
    - list
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
# k8s.mariadb.com - mariadbs (read-only, CC-0047)
# Required for the operator to observe the referenced MariaDB cluster's
# Ready condition and reflect outages in DatabaseReady.
- apiGroups:
    - k8s.mariadb.com
  resources:
    - mariadbs
  verbs:
    - get
    - list
    - watch
# cert-manager.io - certificates (CC-0106)
# Required so the operator can issue the per-Keystone database client
# Certificate (Spec.Database.TLS) and have cert-manager rotate the keypair
# Secret consumed by the Keystone workloads.
- apiGroups:
    - cert-manager.io
  resources:
    - certificates
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - externalsecrets (CC-0017)
# delete is intentionally NOT granted: the operator only manages externalsecret
# lifecycles via owner-references, it never calls r.Delete on them directly.
- apiGroups:
    - external-secrets.io
  resources:
    - externalsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
# external-secrets.io - pushsecrets (CC-0017, CC-0079)
# delete is required so the openbao-finalizer can tear down the fernet-keys
# and credential-keys backup PushSecrets on Keystone CR deletion and rely on
# ESO DeletionPolicy=Delete to purge the kv-v2 paths.
- apiGroups:
    - external-secrets.io
  resources:
    - pushsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - clustersecretstores (read-only, CC-0047)
# Required so the operator can observe the ClusterSecretStore's Ready
# condition and reflect upstream secret-backend (OpenBao) outages in
# SecretsReady. ClusterSecretStore is cluster-scoped; the rule is only
# effective when rendered into the ClusterRole.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
  verbs:
    - get
    - list
    - watch
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
# gateway.networking.k8s.io - httproutes (CC-0065)
# Required to create/update/delete HTTPRoutes that expose Keystone API externally.
- apiGroups:
    - gateway.networking.k8s.io
  resources:
    - httproutes
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# gateway.networking.k8s.io - httproutes/status (read-only, CC-0065)
# Required so the operator can observe the Accepted condition set by the
# upstream Gateway controller and reflect it in HTTPRouteReady.
- apiGroups:
    - gateway.networking.k8s.io
  resources:
    - httproutes/status
  verbs:
    - get
# scheduling.k8s.io - priorityclasses (read-only, CC-0075)
# Required for the webhook to validate that spec.priorityClassName references
# an existing PriorityClass at admission time.
- apiGroups:
    - scheduling.k8s.io
  resources:
    - priorityclasses
  verbs:
    - get
    - list
    - watch
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
