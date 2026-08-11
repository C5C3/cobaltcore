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
# glance.openstack.c5c3.io - glances, glancebackends
# The ControlPlane reconciler projects and Owns a Glance child plus one
# GlanceBackend child per services.glance.backends entry. Both are
# operator-written children, so both get full verbs.
- apiGroups:
    - glance.openstack.c5c3.io
  resources:
    - glances
    - glancebackends
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# placement.openstack.c5c3.io - placements
# The ControlPlane reconciler projects and Owns a Placement child. Placement has
# no satellite kind, so placements is the single operator-written child kind.
- apiGroups:
    - placement.openstack.c5c3.io
  resources:
    - placements
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# barbican.openstack.c5c3.io - barbicans, barbicansecretstores
# The ControlPlane reconciler projects and Owns a Barbican child plus the
# BarbicanSecretStore that points it at its secret backend. Both are
# operator-written children, so both get full verbs.
- apiGroups:
    - barbican.openstack.c5c3.io
  resources:
    - barbicans
    - barbicansecretstores
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# openbao.org - openbaoclusters, openbaotenants
# A managed Barbican secret store gets a dedicated OpenBao instance: the
# OpenBaoCluster the store reads and writes through, and the OpenBaoTenant that
# admits the Barbican service namespace to it.
- apiGroups:
    - openbao.org
  resources:
    - openbaoclusters
    - openbaotenants
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# rbac.authorization.k8s.io - roles, rolebindings (namespaced)
# The dedicated OpenBao instance comes with its own RBAC: the Role/RoleBinding
# pair that lets the barbican-operator mint a bound token for the instance's
# provisioner ServiceAccount.
#
# No list or watch: the reconciler reads these by exact name through the
# manager's UNCACHED API reader (ControlPlaneReconciler.APIReader), so
# controller-runtime never starts a cluster-wide Role/RoleBinding informer for
# the two objects a ControlPlane owns. No update either: every write is a
# Server-Side Apply (patch), and the teardown deletes.
- apiGroups:
    - rbac.authorization.k8s.io
  resources:
    - roles
    - rolebindings
  verbs:
    - get
    - create
    - patch
    - delete
# rbac.authorization.k8s.io - clusterrolebindings (cluster-scoped)
# The one binding that lets the instance pods issue the TokenReview every
# Kubernetes-auth login is validated with. Same verb set and the same uncached
# read as the namespaced pair above.
#
# ACCEPTED RISK: delete on a cluster-scoped binding is not covered by RBAC
# escalation prevention, and the object names are derived per ControlPlane, so
# resourceNames cannot bound them. A compromised operator identity can therefore
# delete any ClusterRoleBinding in the cluster — an authorization OUTAGE, not an
# escalation. The code path never can: deleteBarbicanAuthDelegatorBinding and
# deleteBarbicanEnsembleIn both re-check isControlPlaneChild against the live
# object before deleting. Removing the grant entirely needs the binding to move
# to the openbao-operator, which already creates the ServiceAccount it binds.
- apiGroups:
    - rbac.authorization.k8s.io
  resources:
    - clusterrolebindings
  verbs:
    - get
    - create
    - patch
    - delete
# rbac.authorization.k8s.io - clusterroles (bind on system:auth-delegator only)
# Kubernetes refuses a binding to a ClusterRole whose permissions the author
# does not hold. bind on that single name is the narrow exception that lets the
# reconciler grant the OpenBao instance its TokenReview without holding
# TokenReview itself; no other ClusterRole is bindable.
- apiGroups:
    - rbac.authorization.k8s.io
  resources:
    - clusterroles
  resourceNames:
    - system:auth-delegator
  verbs:
    - bind
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
# core - serviceaccounts/token
# Required so the reconciler may author the TokenRequest Role that lets the
# barbican-operator mint a bound token for a dedicated OpenBao instance:
# Kubernetes only lets an author grant permissions it holds itself.
#
# ACCEPTED RISK, and why it cannot be narrowed. The Role the reconciler writes
# IS resourceNames-scoped to the one "<instance>-provisioner" account (see
# barbicanOpenBaoTokenRole) — the scoping the barbican-operator chart refuses to
# render without. But the escalation-prevention check covers a requested rule
# only from a granted rule whose resourceNames are absent or contain the exact
# name, and the instance name is derived from the ControlPlane, so no static
# resourceNames list can cover every ControlPlane a cluster will ever hold. The
# grant is therefore unrestricted, and anyone reaching this operator's identity
# can mint a bearer token for any ServiceAccount in the cluster.
#
# Two alternatives were rejected. Dropping resourceNames from the projected Role
# and binding a shipped ClusterRole with `bind` moves the verb off this identity,
# but hands the barbican-operator TokenRequest for EVERY account in the Barbican
# namespace — including eso-tenant-auth and the <service>-db-creds accounts that
# read tenant secrets out of OpenBao — which the barbican-operator chart, its
# values schema, and hack/ci-deploy-operator.sh all refuse by design. And
# `escalate` on roles is a strictly wider primitive than the verb it would
# replace. It does not widen what this identity already reaches either: the
# cluster-wide secrets rule above lets it create a legacy
# kubernetes.io/service-account-token Secret for any account and read the token
# out of it. Removing it for real needs the grant to be authored by the
# openbao-operator, which owns the instance the account belongs to.
- apiGroups:
    - ""
  resources:
    - serviceaccounts/token
  verbs:
    - create
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
# discovery.k8s.io - endpointslices
# Read-only on the single well-known EndpointSlice default/kubernetes, which
# carries the addresses the API server answers on. A dedicated Barbican secret
# store projects them into the OpenBaoCluster's
# spec.network.apiServerEndpointIPs, without which the operator-rendered
# NetworkPolicy denies the instance its API-server egress on a CNI that enforces
# against the post-DNAT destination. No list or watch: the name is well known and
# the read goes through the uncached reader.
- apiGroups:
    - discovery.k8s.io
  resources:
    - endpointslices
  verbs:
    - get
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
