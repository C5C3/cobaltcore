{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are horizon-specific, so they stay in this chart rather than the library.
*/}}
{{- define "horizon-operator.rbacRules" -}}
# horizon.openstack.c5c3.io - horizons
- apiGroups:
    - horizon.openstack.c5c3.io
  resources:
    - horizons
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# horizon.openstack.c5c3.io - horizons/status
- apiGroups:
    - horizon.openstack.c5c3.io
  resources:
    - horizons/status
  verbs:
    - get
    - update
    - patch
# horizon.openstack.c5c3.io - horizons/finalizers
- apiGroups:
    - horizon.openstack.c5c3.io
  resources:
    - horizons/finalizers
  verbs:
    - update
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
# core - services, configmaps
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - secrets (read-only)
# The Django SECRET_KEY Secret is ESO-managed; the operator only reads it to
# gate readiness and digest the key material for the rollout annotation.
- apiGroups:
    - ""
  resources:
    - secrets
  verbs:
    - get
    - list
    - watch
# core - events
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
# external-secrets.io - externalsecrets (read-only)
# The SECRET_KEY ExternalSecret is user/infrastructure-provided; the operator
# only consults it to attribute a not-synced Secret in SecretsReady messages.
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
# Horizon selects either the shared cluster-scoped ClusterSecretStore
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
# Required to create/update/delete HTTPRoutes that expose the dashboard externally.
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
