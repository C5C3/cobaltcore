{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change.
Rules mirror the +kubebuilder:rbac markers on the ControlPlane and
CredentialRotation reconcilers, deduplicated across both, plus the
coordination.k8s.io/leases rule required for leader election.
*/}}
{{- define "c5c3-operator.rbacRules" -}}
# c5c3.io - controlplanes
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
# c5c3.io - controlplanes/status
- apiGroups:
    - c5c3.io
  resources:
    - controlplanes/status
  verbs:
    - get
    - update
    - patch
# c5c3.io - controlplanes/finalizers
- apiGroups:
    - c5c3.io
  resources:
    - controlplanes/finalizers
  verbs:
    - update
# c5c3.io - credentialrotations
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
# c5c3.io - credentialrotations/status
- apiGroups:
    - c5c3.io
  resources:
    - credentialrotations/status
  verbs:
    - get
    - update
    - patch
# c5c3.io - secretaggregates (read-only)
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
# k8s.mariadb.com - mariadbs
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
# memcached.c5c3.io - memcacheds
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
# keystone.openstack.c5c3.io - keystones
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
# keystone.openstack.c5c3.io - keystoneidentitybackends
# READ-ONLY: the ControlPlane reconciler watches the federation/domain backends
# attached to its Keystone child to project the Horizon websso choices and the
# Keystone trusted_dashboard. The backends themselves are authored by the
# operator and reconciled by the keystone-operator, never written here.
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystoneidentitybackends
  verbs:
    - get
    - list
    - watch
# horizon.openstack.c5c3.io - horizons
# The ControlPlane reconciler projects and Owns a Horizon child.
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
# openstack.k-orc.cloud - applicationcredentials, services, endpoints, users,
# domains, projects, roles, roleassignments. Minted/owned by reconcileKORC and
# reconcileCatalog; users + domains are imported (unmanaged) so the admin
# ApplicationCredential's UserRef resolves (ensureKORCAdminImports); users +
# projects are also managed/owned by reconcileServiceAccounts
# (spec.korc.serviceAccounts). Roles are imported and RoleAssignments minted for
# the service-account role projection.
- apiGroups:
    - openstack.k-orc.cloud
  resources:
    - applicationcredentials
    - services
    - endpoints
    - users
    - domains
    - projects
    - roles
    - roleassignments
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - externalsecrets, pushsecrets
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
# external-secrets.io - clustersecretstores (read-only)
# Required so the operator can observe the shared cluster store's Ready condition
# and reflect upstream secret-backend outages. A ControlPlane that sets an
# explicit cluster-scoped spec.secretStoreRef reaches OpenBao through it.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
  verbs:
    - get
    - list
    - watch
# external-secrets.io - secretstores (read-write)
# The operator PROVISIONS the per-tenant namespaced SecretStore (openbao-tenant-store)
# it defaults every ControlPlane onto (reconcileESOTenantStore), and observes its
# Ready condition, so it needs the write verbs in addition to the read verbs.
- apiGroups:
    - external-secrets.io
  resources:
    - secretstores
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# generators.external-secrets.io - vaultdynamicsecrets
# Required so reconcileDBCredentials can project the per-ControlPlane
# VaultDynamicSecret generator that issues short-lived DB credentials in
# Dynamic credentials mode.
- apiGroups:
    - generators.external-secrets.io
  resources:
    - vaultdynamicsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# cert-manager.io - certificates
# Required so reconcileDBCredentials can project the per-ControlPlane mTLS client
# Certificate the VaultDynamicSecret generator presents to the OpenBao listener.
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
# core - secrets
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
# core - serviceaccounts
# Required so reconcileDBCredentials can project (and clean up on a Static flip)
# the per-ControlPlane ServiceAccount whose token the VaultDynamicSecret
# generator presents to OpenBao. delete is used by the Dynamic->Static teardown.
- apiGroups:
    - ""
  resources:
    - serviceaccounts
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - namespaces
# Required so reconcileNamespaces can ensure the namespaces a service is placed
# in via spec.services.<svc>.namespace: create for the Managed lifecycle, delete
# for the teardown that follows it, get/list/watch for both lifecycles (an
# External namespace is only ever verified, never mutated). A ControlPlane with
# no namespace assignments never exercises create or delete.
- apiGroups:
    - ""
  resources:
    - namespaces
  verbs:
    - get
    - list
    - watch
    - create
    - delete
# core - events
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
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
