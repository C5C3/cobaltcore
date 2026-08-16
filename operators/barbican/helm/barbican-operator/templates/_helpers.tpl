{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are barbican-specific, so they stay in this chart rather than the library.
They mirror the union of the kubebuilder RBAC markers in
operators/barbican/internal/controller/barbican_controller.go and
barbicansecretstore_controller.go; the leases rule is the one addition,
required by the manager's leader election.
*/}}
{{- define "barbican-operator.rbacRules" -}}
# barbican.openstack.c5c3.io - barbicans
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicans
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# barbican.openstack.c5c3.io - barbicans/status
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicans/status
  verbs:
    - get
    - update
    - patch
# barbican.openstack.c5c3.io - barbicans/finalizers
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicans/finalizers
  verbs:
    - update
# barbican.openstack.c5c3.io - barbicansecretstores
# Users create store CRs; the operator lists and watches them to assemble the
# aggregate Barbican secret-store configuration and never creates or deletes
# one itself. update covers the placed-store teardown marks: before minting
# credentials onto a target cluster, the store controller records the
# children-cluster annotation and the remote-children finalizer on the CR,
# and releases that finalizer again on delete — all plain Updates of the
# main resource.
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicansecretstores
  verbs:
    - get
    - list
    - watch
    - update
# barbican.openstack.c5c3.io - barbicansecretstores/status
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicansecretstores/status
  verbs:
    - get
    - update
    - patch
# barbican.openstack.c5c3.io - barbicansecretstores/finalizers
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicansecretstores/finalizers
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
# NOTE: create on serviceaccounts/token is deliberately NOT part of this rule
# set. It is granted per namespace by templates/tokenrequest-rbac.yaml — see the
# comment there for why it must never reach a ClusterRole.
# batch - jobs, cronjobs
# jobs covers the db-sync Job; cronjobs covers the recurring database clean-up
# the DBClean sub-reconciler projects.
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
# Barbican selects either the shared cluster-scoped ClusterSecretStore
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
# openbao.org - openbaoclusters (read-only)
# A managed BarbicanSecretStore names the OpenBaoCluster it provisions against;
# the store controller reads its Available condition, trust bundle, and
# provisioner ServiceAccount, and watches it so an instance turning Available
# wakes the store.
- apiGroups:
    - openbao.org
  resources:
    - openbaoclusters
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
# Required to create/update/delete HTTPRoutes that expose the Barbican API externally.
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
