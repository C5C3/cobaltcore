{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are glance-specific, so they stay in this chart rather than the library.
*/}}
{{- define "glance-operator.rbacRules" -}}
# glance.openstack.c5c3.io - glances
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glances
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# glance.openstack.c5c3.io - glances/status
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glances/status
  verbs:
    - get
    - update
    - patch
# glance.openstack.c5c3.io - glances/finalizers
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glances/finalizers
  verbs:
    - update
# glance.openstack.c5c3.io - glancebackends (read-only)
# Users create backend CRs; the operator only lists and watches them to
# assemble the aggregate Glance store configuration.
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glancebackends
  verbs:
    - get
    - list
    - watch
# glance.openstack.c5c3.io - glancebackends/status
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glancebackends/status
  verbs:
    - get
    - update
    - patch
# apps - deployments
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
# core - services, configmaps, secrets
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
    - secrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - events
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
# batch - jobs
- apiGroups:
    - batch
  resources:
    - jobs
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# k8s.mariadb.com - databases, users, grants
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
# k8s.mariadb.com - mariadbs (read-only)
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
# external-secrets.io - externalsecrets (read-only)
# The database and service-user credentials Secrets are ESO-managed; the
# operator only reads the ExternalSecrets to attribute a not-synced Secret in
# SecretsReady messages.
- apiGroups:
    - external-secrets.io
  resources:
    - externalsecrets
  verbs:
    - get
    - list
    - watch
# external-secrets.io - clustersecretstores, secretstores (read-only)
# Required so the operator can observe the selected store's Ready condition
# and reflect upstream secret-backend (OpenBao) outages in SecretsReady. A
# Glance selects either the shared cluster-scoped ClusterSecretStore
# (default) or a namespaced SecretStore via spec.secretStoreRef, so both kinds
# must be watchable.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
    - secretstores
  verbs:
    - get
    - list
    - watch
# policy - poddisruptionbudgets
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
# autoscaling - horizontalpodautoscalers
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
# networking.k8s.io - networkpolicies
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
# gateway.networking.k8s.io - httproutes
# Required to create/update/delete HTTPRoutes that expose the Glance API externally.
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
# gateway.networking.k8s.io - httproutes/status (read-only)
# Required so the operator can observe the Accepted condition set by the
# upstream Gateway controller and reflect it in HTTPRouteReady.
- apiGroups:
    - gateway.networking.k8s.io
  resources:
    - httproutes/status
  verbs:
    - get
# scheduling.k8s.io - priorityclasses (read-only)
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
# coordination.k8s.io - leases for leader election
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
