---
title: Multi-Tenant Deployment
quadrant: operator
---

# Multi-Tenant Deployment

This guide is for deploying operators in namespace-scoped mode, using Keystone as the
example. In this mode an operator uses a `Role` / `RoleBinding` instead of
`ClusterRole` / `ClusterRoleBinding` and restricts its cache, watches, and
reconciliation to a single namespace. The service operator charts and the
ControlPlane operator chart expose the same `rbac.namespaceScoped` setting.

This guide covers running the Keystone operator itself namespace-scoped: one
operator instance confined to one namespace. That is distinct from the higher-level
tenancy unit, the **`ControlPlane` CR**. A ControlPlane CR is namespaced by
default, but its operator runs cluster-wide (by default). When enabled, its
validating webhook enforces at most one ControlPlane per namespace. Give each tenant
its own namespace with a ControlPlane and per-CR-scoped credentials (admin
password, K-ORC application credential, Keystone keys). To create tenants as
ControlPlanes, start from
the [ControlPlane Quick Start](../quick-start-controlplane.md); the namespace-scoped
operator RBAC described here is the complementary, lower-level concern.

For a control plane confined to one namespace, namespace-scoped operators can
confine Secret access without cluster permissions. Set
`rbac.namespaceScoped: true`.
This replaces an operator's
cluster-wide `ClusterRole` with a `Role` bound to a single namespace. The
operator pod can then only access that namespace's Secrets. The
[Security trade-off](#security-trade-off-the-cluster-wide-rbac-default) below
explains the privilege-escalation path this closes, and why it stays open for
the c5c3-operator.

The chart still ships cluster-wide (`rbac.namespaceScoped: false`) by default
because some capabilities still need cluster scope; see
[When cluster-wide RBAC is still required](#when-cluster-wide-rbac-is-still-required).
Adopt namespace-scoped mode when your use-case fits the deployment.

## Prerequisites

::: info Devstack
This guide builds on the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack for the cluster, CRDs, and shared infrastructure (MariaDB, Memcached, External Secrets Operator). Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step. This guide then
re-deploys the keystone-operator namespace-scoped (the steps below). Treat the
tutorial as the cluster-and-infrastructure baseline for this installation.
:::

Before deploying the operator in namespace-scoped mode, ensure:

1. **CRDs are installed cluster-wide.** Keystone CRDs (`keystones.keystone.openstack.c5c3.io`)
  are always cluster-scoped resources; they cannot be installed per-namespace. A
  cluster-admin must install the CRDs before any operator instance
  can start. Install them once with `kubectl apply -f` from a privileged context
  as shown in [CRD installation](#crd-installation).
2. **Target namespace exists.** The namespace into which you deploy the operator
  must already exist, or pass `--create-namespace` to `helm upgrade -i`.
3. **Infrastructure dependencies are reachable.** MariaDB, Memcached, and
   External Secrets Operator services must be accessible from the tenant
   namespace (see [Multiple instances](#multiple-instances-in-different-namespaces)).

---

## When to use namespace-scoped mode

- **Multi-tenant clusters:** multiple teams share a cluster and each team
  operates its own OpenStack control plane in an isolated namespace.
- **Least-privilege requirements:** security policy mandates that workloads
  must not hold cluster-wide permissions.
- **Multiple control planes:** you need several independent ControlPlanes,
  each in a different namespace, with operators confined to those namespaces.

---

## When cluster-wide RBAC is still required

Keep the default (`rbac.namespaceScoped: false`) when any of these apply. Each
needs cluster scope, which namespace-scoped mode cannot provide:

- **Cross-namespace CR management:** a single operator instance reconciles
  `Keystone` (or `ControlPlane`) CRs in more than one namespace. A
  namespace-scoped operator only watches and reconciles its own namespace.
- **Admission webhooks:** the defaulting and validating webhooks register
  through cluster-scoped `ValidatingWebhookConfiguration` /
  `MutatingWebhookConfiguration` objects, which only a `ClusterRole` can manage
  (see [Webhook caveat](#webhook-caveat)).
- **`ClusterSecretStore` reads** — a CR reads its secrets through a
  `ClusterSecretStore`, such as the shared `openbao-cluster-store`. A
  namespace-scoped operator refuses such a CR (see
  [Secret stores in namespace-scoped mode](#secret-stores-in-namespace-scoped-mode)).
- **The c5c3-operator, ovn-operator and neutron-operator charts** — these
  charts refuse `rbac.namespaceScoped=true` at render time. The c5c3 and ovn
  operators watch cluster-scoped kinds, and the neutron operator reads across
  namespaces.

---

## Security trade-off: the cluster-wide RBAC default

The default (`rbac.namespaceScoped: false`) binds the operator's ServiceAccount
to a `ClusterRole`. Among its rules, that ClusterRole grants:

- `get` / `list` / `watch` / `create` / `update` / `patch` / `delete` on
  `secrets` in every namespace, and
- `create` on `serviceaccounts` plus full CRUD on `roles` and `rolebindings`,
  which the operator needs to mint the per-CronJob rotation RBAC described
  [below](#contrast-the-per-cronjob-rotation-rbac).

### Privilege-escalation path

A compromised operator pod or a leaked ServiceAccount token can therefore:

1. Read every Secret in the cluster.
2. Make the compromise durable. An attacker can bind permissions to a subject
  they control, turning a transient pod compromise into a standing issue that
  outlives the pod.

The ControlPlane operator widens the blast radius further: it projects the
OpenStack admin password in cleartext into a `clouds.yaml` `Secret` in each
tenant's child namespace (see
[ControlPlane Reconciler → RBAC Permissions](../reference/c5c3/controlplane-reconciler.md#rbac-permissions)).
Cluster-wide Secret read access exposes all of those projected passwords.

The c5c3-operator chart refuses `rbac.namespaceScoped: true` (see
[When cluster-wide RBAC is still required](#when-cluster-wide-rbac-is-still-required)),
so the ControlPlane operator's cluster-wide Secret and RoleBinding grants cannot
be scoped down. Protect it by other means: limit who can exec into its pod or
read its ServiceAccount token, run it on dedicated nodes, and audit the API
requests its ServiceAccount makes from anywhere other than that pod.

### Contrast: the per-CronJob rotation RBAC

Each rotation CronJob has a namespaced `Role`. Its `resourceNames` rules grant
`get` on the push-source `Secret` and `get` and `patch` on the staging `Secret`.
It cannot write to a Secret consumed by a privileged workload. Namespace-scoping
the operator also limits its RBAC to the namespace it manages.

### Why the cluster-wide Secret rule cannot simply be narrowed

A natural question is whether the cluster-wide `secrets` rule could be pinned to
specific names or labels. For the cluster-wide deployment model it can not.

- **`resourceNames` does not apply to `list` / `watch`.**
- **The names are dynamic.** Managed Secrets (the Fernet keys, the
  credential keys, the database-connection Secret, the projected `clouds.yaml`)
  are named after each CR and spread across namespaces.
- **RBAC has no label or field selectors.**

The supported way to restrict the blast radius is therefore to **reduce the
scope**. `rbac.namespaceScoped: true` confines both the RBAC grant and the
informer cache to a single namespace.

---

## Helm values

Two values control namespace-scoped mode:

| Value | Default | Description |
| --- | --- | --- |
| `rbac.namespaceScoped` | `false` | Deploy namespace-scoped `Role` / `RoleBinding` instead of `ClusterRole` / `ClusterRoleBinding`. Passes `--namespace` to the operator binary to restrict its cache and watches. |
| `webhook.enabled` | `true` | Must be set to `false` when `rbac.namespaceScoped` is `true` (see [Webhook caveat](#webhook-caveat)). |

Minimal values override:

```yaml
# values-namespace-scoped.yaml
rbac:
  namespaceScoped: true

webhook:
  enabled: false
```

---

## Webhook caveat

When `rbac.namespaceScoped` is `true`, you **must** disable webhooks by setting
`webhook.enabled: false`.

**Why:** Kubernetes admission webhooks are registered via
`ValidatingWebhookConfiguration` and `MutatingWebhookConfiguration`, which are
**cluster-scoped** resources. A namespace-scoped operator does not have
permission to create or manage cluster-scoped resources, so webhook
registration will fail.

Disabling the c5c3 operator's validating webhook also removes admission-time
enforcement of the one-ControlPlane-per-namespace rule. Keep that constraint
in your deployment process until a separate webhook deployment is available.

With webhooks disabled the following admission-time behaviors are lost:

| Behavior | Impact |
| --- | --- |
| Defaulting webhook | Zero-valued fields (`replicas: 0`, empty `cache.backend`, etc.) are no longer auto-filled. You must set all required fields explicitly in your `Keystone` CRs. |
| Validating webhook | Server-side validation of cron expressions, duplicate plugin sections, and resource request/limit ordering is skipped. Invalid CRs will be accepted by the API server and fail at reconciliation time instead of at admission time. |

CRD-level CEL validation rules remain active.
These rules cover structural constraints such as `database` mutual exclusivity,
`autoscaling` metric requirements, and minimum-value checks, as well as the
immutability transition rules.

---

## Secret stores in namespace-scoped mode

A namespace-scoped operator cannot read a `ClusterSecretStore`. The kind is
cluster-scoped, so the `Role` the chart renders grants nothing for it, and an
operator started with `--namespace` registers no watch on it. Every CR such an
operator reconciles sets `spec.secretStoreRef` to a namespaced `SecretStore`
in the CR's own namespace:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: team-alpha
spec:
  # …
  secretStoreRef:
    kind: SecretStore
    name: openbao-tenant-store
```

A CR that omits the field resolves to the shared `ClusterSecretStore`
`openbao-cluster-store`. The operator refuses it, and any CR that names a
`ClusterSecretStore`, without reading the store. The CR reports
`SecretsReady=False` with reason `ClusterSecretStoreUnsupported` and a message
that names the store and the namespace, and the operator checks it again every
15 seconds. Setting `spec.secretStoreRef` to a `SecretStore` clears the
condition.

`setup-eso-tenant.sh` creates `openbao-tenant-store` in a namespace; the command
is under [Migrating an existing deployment](#migrating-an-existing-deployment).

---

## Example: namespace-scoped install

::: info Chart scope
These commands use the Keystone chart. The other service operator charts and
the c5c3 ControlPlane operator chart also expose `rbac.namespaceScoped`; each
operator needs its own chart, image, CRDs, and namespace. A namespaced
`ControlPlane` CR does not itself change the scope of its operator.
:::

::: danger Known ClusterSecretStore watch failure
The current Keystone controller watches `ClusterSecretStore` even in namespace-scoped
mode. Its namespaced `Role` cannot list that cluster-scoped resource, so the
watch repeatedly fails with `clustersecretstores.external-secrets.io is forbidden`.
Pod readiness alone does not establish that reconciliation works. Do not use
this mode for Keystone until the watch and RBAC contract are fixed and a
Keystone CR reaches `Ready=True` in a namespace-scoped test.
:::

Deploy the operator into the `team-alpha` namespace with namespace-scoped RBAC.
Build the operator image and load it into the kind cluster first. This mirrors
the install form of the guide's namespace-scoped-rbac chainsaw suite (a local
`dev` image with `pullPolicy=Never`, no registry needed):

```bash
make docker-build OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name cobaltcore
```

```bash
helm dep up operators/keystone/helm/keystone-operator
helm upgrade -i keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-alpha --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait --timeout 120s
```

This creates the following RBAC resources in `team-alpha` (not at cluster
scope):

```
Role/keystone-operator          (namespace: team-alpha)
RoleBinding/keystone-operator   (namespace: team-alpha)
```

The operator Deployment receives the `--namespace=team-alpha` argument,
restricting its controller-runtime cache and watches to that namespace.

---

## Multiple instances in different namespaces

You can install the operator multiple times, once per namespace, with each
instance independently managing its own `Keystone` CRs:

```bash
helm dep up operators/keystone/helm/keystone-operator

# Team Alpha
helm upgrade -i keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-alpha --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never

# Team Beta
helm upgrade -i keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-beta --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never
```

Each instance only watches and reconciles resources in its own namespace.
There is no cross-namespace interference because:

1. The `Role` / `RoleBinding` grants permissions only within the release
   namespace.
2. The `--namespace` flag restricts the controller-runtime informer cache to
   that namespace.
3. Leader election leases are namespace-scoped, so each instance elects its
   own leader independently.

Infrastructure dependencies (MariaDB, Memcached, External Secrets Operator)
must be accessible from each tenant namespace. Depending on your cluster setup,
this may require cross-namespace `Service` references or per-namespace
infrastructure stacks.

---

## CRD installation

Custom Resource Definitions (CRDs) are always cluster-scoped. Even when the operator itself runs in
namespace-scoped mode, the CRDs must be installed at the cluster level by a
user with cluster-admin privileges.

```bash
# Install CRDs directly from the chart's crds/ directory
kubectl apply --server-side -f \
  operators/keystone/helm/keystone-operator/crds/
```

If you manage CRDs separately (e.g., via a GitOps pipeline or a dedicated
CRD-management chart), ensure they are applied before deploying any
operator instances. A missing CRD causes the operator to crash on startup.

::: tip Local chart path vs. published OCI chart
The `helm upgrade -i` examples above use the in-repo chart path
`operators/keystone/helm/keystone-operator/`, which assumes a checked-out repository.
For deployments off a checkout, use the published OCI chart instead:
`oci://ghcr.io/c5c3/charts/keystone-operator`.
:::

---

## Per-ControlPlane secret stores and OpenBao identities

Every `ControlPlane` and the service children it owns reach OpenBao through
namespaced `openbao-tenant-store` stores. The c5c3 operator provisions a store
in each namespace used by the ControlPlane and its children and projects the
store reference onto the children. Each store authenticates to OpenBao as the
`eso-tenant` Kubernetes auth role, and the `eso-tenant` templated policy scopes
every readable and writable path to the caller's own namespace. A tenant token
in namespace `team-alpha` can therefore only reach `team-alpha`'s key
and bootstrap material and is denied on any other tenant's paths. OpenBao
isolates one control plane's secret material from another.

The figure shows the tenant store of one ControlPlane namespace beside the
shared cluster store.

![Secret flow on the management cluster. OpenBao in shared-services holds a KV engine and a database engine, and the External Secrets Operator moves three kinds of secret. Read: an ExternalSecret copies a value from the KV engine through a secret store into a Secret that pods and Jobs consume. Write-back: a PushSecret copies a Secret an operator wrote through the store into the KV engine. Dynamic: a VaultDynamicSecret generator draws a short-lived MariaDB user from the database engine with a login of its own and no store. A ControlPlane namespace uses the SecretStore openbao-tenant-store, which the c5c3-operator creates and which logs in with the role eso-tenant. The ClusterSecretStore openbao-cluster-store, with the role eso-management, serves standalone service CRs in the openstack namespace.](../diagrams/secrets-flow.svg)

This is the **enforced default**: you configure nothing, and existing
operator-managed ControlPlanes migrate onto it on operator upgrade. The shared
cluster-scoped store `openbao-cluster-store` no longer carries any
per-ControlPlane write grant or Keystone read. It is restricted to the
namespaces hosting the static infrastructure ExternalSecrets and grants only the
genuinely shared `bootstrap` and `infrastructure` reads.

`spec.secretStoreRef` is an override in case you manage the
store yourself:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: team-alpha
spec:
  # …
  # Optional. OMIT this to get the operator-provisioned per-tenant store (the
  # default).
  secretStoreRef:
    kind: SecretStore
    name: my-own-store
```

- **Omitted (the default).** The operator provisions the per-tenant identity:
  the `ServiceAccount` (`eso-tenant-auth`), the cert-manager mTLS `Certificate`,
  and the namespaced `SecretStore` (`openbao-tenant-store`) as owned children
  and routes the ControlPlane and its children through it.
- **Set.** The operator provisions nothing and uses the store you name (a
  namespaced `SecretStore` you manage, or the shared `openbao-cluster-store`).
  The reference is projected onto the service children, so this
  is the single place you configure it.

### Migrating an existing deployment

::: warning Upgrade from the shared store
Older deployments used the shared `openbao-cluster-store` for per-ControlPlane
secret traffic. Its wildcard write grants allowed a tenant's ESO identity to
access another tenant's keys. The new default uses a namespaced
`openbao-tenant-store` with a per-tenant identity. Follow the migration steps
below when upgrading an existing deployment.
:::

**Operator-managed ControlPlanes** migrate from the shared cluster store to the
per-tenant namespaced store automatically. Upgrade the operators;
each ControlPlane provisions its per-tenant store and re-points its ExternalSecrets
and PushSecrets in place. On a cluster whose OpenBao was bootstrapped before
this feature, re-run `deploy/openbao/bootstrap/setup-auth.sh` and
`setup-policies.sh` so the `eso-tenant` role and policy exist. Until they do, the
per-tenant stores stay `NotReady` and reconciliation of new secret objects is
gated, but existing Secrets keep serving and no key material is lost (a `403`
never deletes anything).

::: danger Do not delete a ControlPlane mid-migration
Deleting a ControlPlane while its PushSecrets cannot reach OpenBao leaves the
`DeletionPolicy=Delete` finalizer looping on `403` and the CR stuck in
`Terminating`. Complete the OpenBao bootstrap re-run first.
:::

**Standalone service CRs** have no ControlPlane operator above them to provision
a store. Operators for Keystone, Horizon, Barbican, Cinder, Glance, Neutron,
Nova, and Placement expose `spec.secretStoreRef`; set it explicitly when moving
from the shared store. The OpenBao side (the `eso-tenant` auth role and policy)
is created once at bootstrap by `setup-auth.sh` / `setup-policies.sh`; the
in-cluster side is created per namespace by `setup-eso-tenant.sh`:

```bash
# Run once per tenant namespace, then wait for the SecretStore to be Ready.
deploy/openbao/bootstrap/setup-eso-tenant.sh team-alpha
kubectl wait --for=condition=Ready secretstore/openbao-tenant-store \
  -n team-alpha --timeout=120s
```

Then set `spec.secretStoreRef: {kind: SecretStore, name: openbao-tenant-store}`
on each standalone service CR that uses OpenBao. The tenant store is provisioned
per namespace, not per service.

A ControlPlane's fernet and credential keys are **irreplaceable**: the credential
keys decrypt every application credential, EC2 credential, and TOTP secret, and
their OpenBao backup is bound to a PushSecret that deletes the remote copy when
the binding goes away. Switching stores moves this material; it never
re-creates it: the operator updates the backup PushSecrets **in place**
(unchanged name and OpenBao path) so a store switch only re-points the identity.

The per-ControlPlane database and cache need no additional work: the
infrastructure a ControlPlane owns is already created in the ControlPlane's own
namespace, so two ControlPlanes already get two instances.

---

## Further reading

- [ControlPlane Quick Start](../quick-start-controlplane.md): standing up a tenant as a `ControlPlane` CR (the one-per-namespace tenancy aggregate).
- [Enable the Keystone Operator NetworkPolicy](./keystone/enable-keystone-operator-networkpolicy.md): confine the namespace-scoped operator's egress.
- [Helm Values Schema](../reference/backend/helm-values-schema.md): the full `rbac.*` / `webhook.*` value reference.

## Tested by

The namespace-scoped install and the two-ControlPlanes-in-two-namespaces tenancy
this guide describes are asserted on the CI e2e kind cluster by these chainsaw
suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/namespace-scoped-rbac
chainsaw test --test-dir tests/e2e/c5c3/multi-controlplane
```
