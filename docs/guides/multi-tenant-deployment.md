---
title: Multi-Tenant Deployment
quadrant: operator
---

# Multi-Tenant Deployment

Guide for deploying the Keystone operator in namespace-scoped mode.
In this mode the operator uses a `Role` / `RoleBinding` instead of
`ClusterRole` / `ClusterRoleBinding` and restricts its cache, watches, and
reconciliation to a single namespace.

::: tip Two different "multi-tenant" axes
This guide covers running the **Keystone operator itself** namespace-scoped — one
operator instance confined to one namespace. That is distinct from the higher-level
tenancy unit, the **`ControlPlane` CR**: a validating webhook enforces **at most one
`ControlPlane` per namespace**, so each tenant lives in its own namespace with its own
ControlPlane and per-CR-scoped credentials (admin password, K-ORC application
credential, Keystone keys). If you are standing tenants up as ControlPlanes, start from
the [ControlPlane Quick Start](../quick-start-controlplane.md); the namespace-scoped
operator RBAC described here is the complementary, lower-level concern.
:::

::: tip Recommended for single-namespace production
When a control plane is confined to **one namespace**, deploy the operator
namespace-scoped (`rbac.namespaceScoped: true`). This replaces the operator's
cluster-wide `ClusterRole` — which grants read/write on **every** `Secret` in
**every** namespace — with a `Role` bound to a single namespace, so a
compromised operator pod can reach only that namespace's Secrets. The
[Security trade-off](#security-trade-off-the-cluster-wide-rbac-default) below
explains the privilege-escalation path this closes.

The chart still ships cluster-wide (`rbac.namespaceScoped: false`) by default
because some capabilities still need cluster scope — see
[When cluster-wide RBAC is still required](#when-cluster-wide-rbac-is-still-required).
Adopt namespace-scoped mode when your deployment fits in one namespace.
:::

## Prerequisites

::: info Devstack
This guide builds on the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack for the cluster, CRDs, and shared infrastructure (MariaDB, Memcached, External Secrets Operator). Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step. This guide then
re-deploys the keystone-operator **namespace-scoped** (the steps below), so treat
the tutorial as the cluster-and-infrastructure baseline rather than its
cluster-wide operator install.
:::

Before deploying the operator in namespace-scoped mode, ensure:

1. **CRDs are installed cluster-wide.** Keystone CRDs (`keystones.keystone.openstack.c5c3.io`)
   are always cluster-scoped resources — they cannot be installed per-namespace. A
   cluster-admin must install the CRDs before any namespace-scoped operator instance
   can start. Typically CRDs are installed once via `helm install` with
   `--include-crds` or `kubectl apply -f` from a privileged context.
2. **Target namespace exists.** The namespace into which you deploy the operator
   must already exist, or pass `--create-namespace` to `helm install`.
3. **Infrastructure dependencies are reachable.** MariaDB, Memcached, and
   External Secrets Operator services must be accessible from the tenant
   namespace (see [Multiple instances](#multiple-instances-in-different-namespaces)).

---

## When to use namespace-scoped mode

- **Multi-tenant clusters** — multiple teams share a cluster and each team
  operates its own OpenStack control plane in an isolated namespace.
- **Least-privilege requirements** — security policy mandates that workloads
  must not hold cluster-wide permissions.
- **Multiple operator instances** — you need several independent Keystone
  operators, each managing a different namespace.

---

## When cluster-wide RBAC is still required

Keep the default (`rbac.namespaceScoped: false`) when any of these apply — each
needs cluster scope, which namespace-scoped mode cannot provide:

- **Cross-namespace CR management** — a single operator instance reconciles
  `Keystone` (or `ControlPlane`) CRs in more than one namespace. A
  namespace-scoped operator only watches and reconciles its own namespace.
- **Admission webhooks** — the defaulting and validating webhooks register
  through cluster-scoped `ValidatingWebhookConfiguration` /
  `MutatingWebhookConfiguration` objects, which only a `ClusterRole` can manage
  (see [Webhook caveat](#webhook-caveat)).

---

## Security trade-off: the cluster-wide RBAC default

The default (`rbac.namespaceScoped: false`) binds the operator's ServiceAccount
to a **`ClusterRole`**. Among its rules, that ClusterRole grants:

- `get` / `list` / `watch` / `create` / `update` / `patch` / `delete` on
  **`secrets`** in **every** namespace, and
- `create` on `serviceaccounts` plus full CRUD on `roles` and `rolebindings` —
  which the operator needs to mint the per-CronJob rotation RBAC described
  [below](#contrast-the-per-cronjob-rotation-rbac).

### Privilege-escalation path

A compromised operator pod — or a leaked ServiceAccount token — can therefore:

1. **Read every Secret in the cluster.** Database passwords, TLS keys, service
   credentials, and the OpenStack admin password, in any namespace.
2. **Make the compromise durable.** Because the same ClusterRole grants `create`
   on `rolebindings`, an attacker can bind an existing `Role` (or the operator's
   own permissions) to a subject they control, turning a transient pod
   compromise into a standing, cluster-wide secret-read credential that outlives
   the pod being killed.

The ControlPlane operator widens the blast radius further: it projects the
OpenStack admin password **in cleartext** into a `clouds.yaml` `Secret` in each
tenant's child namespace (see
[ControlPlane Reconciler → RBAC Permissions](../reference/c5c3/controlplane-reconciler.md#rbac-permissions)).
Cluster-wide Secret read access exposes every one of those projected passwords.

### Contrast: the per-CronJob rotation RBAC

The RBAC the operator *generates* for its rotation CronJobs is the model to
follow. Each CronJob gets a namespaced `Role` with `get` on exactly the
push-source `Secret` and `get` + `patch` on exactly the staging `Secret`, both
pinned by `resourceNames`; the CronJob never holds write access to a Secret a
privileged workload consumes. Namespace-scoping the operator brings the
operator's *own* footprint closer to that least-privilege shape.

### Why the cluster-wide Secret rule cannot simply be narrowed

A natural question is whether the cluster-wide `secrets` rule could be pinned to
specific names or labels instead of switching to namespace scope. For the
cluster-wide deployment model, it cannot:

- **`resourceNames` does not apply to `list` / `watch`.** The operator's
  controller-runtime cache `list`s and `watch`es Secrets to stay in sync, and an
  RBAC rule carrying `resourceNames` does not authorize collection
  (`list` / `watch`) requests — pinning names would break the cache.
- **The names are dynamic and per-CR.** Managed Secrets (the Fernet keys, the
  credential keys, the database-connection Secret, the projected `clouds.yaml`)
  are named after each CR and spread across namespaces, so there is no static
  set of names to enumerate.
- **RBAC has no label or field selectors.** Authorization is all-or-nothing for
  a resource type within the granted scope. The informer cache *can* be
  label-filtered, but that reduces memory, not the ServiceAccount's authority.

The supported way to bound the blast radius is therefore to **reduce the
scope**, not the rule: `rbac.namespaceScoped: true` confines both the RBAC grant
and the informer cache to a single namespace.

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

**Trade-off:** With webhooks disabled the following admission-time behaviors
are lost:

| Behavior | Impact |
| --- | --- |
| Defaulting webhook | Zero-valued fields (`replicas: 0`, empty `cache.backend`, etc.) are no longer auto-filled. You must set all required fields explicitly in your `Keystone` CRs. |
| Validating webhook | Server-side validation of cron expressions, duplicate plugin sections, and resource request/limit ordering is skipped. Invalid CRs will be accepted by the API server and fail at reconciliation time instead of at admission time. |

CRD-level CEL validation rules remain active regardless of webhook status.
These rules cover structural constraints such as `database` mutual exclusivity,
`autoscaling` metric requirements, and minimum-value checks, as well as the
immutability transition rules (`database.name`, the database mode,
`bootstrap.adminUser`, `bootstrap.region`) — those are enforced by the API
server itself, so they hold even with the webhook disabled.

---

## Example: namespace-scoped install

Deploy the operator into the `team-alpha` namespace with namespace-scoped RBAC.
Build the operator image and load it into the kind cluster first — this mirrors
the install form of the guide's namespace-scoped-rbac chainsaw suite (a local
`dev` image with `pullPolicy=Never`, no registry needed):

```bash
make docker-build OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name forge
```

```bash
helm install keystone-operator \
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
# Team Alpha
helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-alpha --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false

# Team Beta
helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-beta --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false
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

Custom Resource Definitions (CRDs) are **always cluster-scoped** — Kubernetes
does not support namespace-scoped CRDs. Even when the operator itself runs in
namespace-scoped mode, the CRDs must be installed at the cluster level by a
user with cluster-admin privileges.

```bash
# Install CRDs directly from the chart's crds/ directory
kubectl apply --server-side -f \
  operators/keystone/helm/keystone-operator/crds/
```

If you manage CRDs separately (e.g., via a GitOps pipeline or a dedicated
CRD-management chart), ensure they are applied **before** deploying any
namespace-scoped operator instance. A missing CRD causes the operator to crash
on startup because the informer cache cannot watch the unknown resource type.

::: tip Local chart path vs. published OCI chart
The `helm install` examples above use the in-repo chart path
`operators/keystone/helm/keystone-operator/`, which assumes a checked-out repository.
For deployments off a checkout, use the published OCI chart instead —
`oci://ghcr.io/c5c3/charts/keystone-operator` — with the same `--set` flags.
:::

---

## Per-ControlPlane secret stores and OpenBao identities

Every `ControlPlane` — and the `Keystone` / `Horizon` children it owns —
reaches OpenBao through a **per-tenant namespaced store**, `openbao-tenant-store`,
that the c5c3 operator provisions in the ControlPlane's own namespace and
projects onto its children. That store authenticates against OpenBao as the
`eso-tenant` Kubernetes auth role, and the `eso-tenant` templated policy scopes
every readable and writable path to the caller's own namespace. A tenant token
in namespace `team-alpha` can therefore only reach `team-alpha`'s Keystone key
and bootstrap material and is **denied** on any other tenant's paths — so
OpenBao itself, not a naming convention, isolates one control plane's secret
material from another.

This is the **enforced default**: you configure nothing, and existing
operator-managed ControlPlanes migrate onto it on operator upgrade. The shared
cluster-scoped store `openbao-cluster-store` no longer carries any
per-ControlPlane write grant or Keystone read — it is restricted to the
namespaces hosting the static infrastructure ExternalSecrets and grants only the
genuinely shared `bootstrap` and `infrastructure` reads.

`spec.secretStoreRef` is an **override**, for the rare case where you manage the
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
  # default). Set it only to point at a store you manage yourself, e.g.:
  secretStoreRef:
    kind: SecretStore
    name: my-own-store
```

- **Omitted (the default).** The operator provisions the per-tenant identity —
  the `ServiceAccount` (`eso-tenant-auth`), the cert-manager mTLS `Certificate`,
  and the namespaced `SecretStore` (`openbao-tenant-store`) — as owned children
  and routes the ControlPlane and its children through it. Nothing to do.
- **Set.** The operator provisions nothing and uses the store you name (a
  namespaced `SecretStore` you manage, or the shared `openbao-cluster-store`).
  The reference is projected onto the `Keystone` and `Horizon` children, so this
  is the single place you configure it.

### Migrating an existing deployment

::: warning The "keeps working unchanged across upgrades" promise is withdrawn
Before this change the shared store carried wildcard write grants that let one
tenant's ESO identity read and overwrite another tenant's irreplaceable key
material. Retiring those grants means the shared-store default no longer works
for per-ControlPlane secret traffic. Upgrading follows a defined path instead:
:::

**Operator-managed ControlPlanes** migrate automatically. Upgrade the operators;
each ControlPlane provisions its per-tenant store and re-points its ExternalSecrets
and PushSecrets in place. On a cluster whose OpenBao was bootstrapped **before**
this feature, re-run `deploy/openbao/bootstrap/setup-auth.sh` and
`setup-policies.sh` so the `eso-tenant` role and policy exist. Until they do, the
per-tenant stores stay `NotReady` and reconciliation of new secret objects is
gated — but existing Secrets keep serving and no key material is lost (a `403`
never deletes anything).

::: danger Do not delete a ControlPlane mid-migration
Deleting a ControlPlane while its PushSecrets cannot reach OpenBao leaves the
`DeletionPolicy=Delete` finalizer looping on `403` and the CR stuck in
`Terminating`. Complete the OpenBao bootstrap re-run first.
:::

**Standalone Keystone / Horizon** deployments have no ControlPlane operator above
them to provision a store, so they onboard the per-tenant identity manually and
set the ref explicitly. The OpenBao side — the `eso-tenant` auth role and policy
— is created once at bootstrap by `setup-auth.sh` / `setup-policies.sh`; the
in-cluster side is created per namespace by `setup-eso-tenant.sh`:

```bash
# Run once per tenant namespace, then wait for the SecretStore to be Ready.
deploy/openbao/bootstrap/setup-eso-tenant.sh team-alpha
kubectl wait --for=condition=Ready secretstore/openbao-tenant-store \
  -n team-alpha --timeout=120s
```

Then set `spec.secretStoreRef: {kind: SecretStore, name: openbao-tenant-store}`
on the standalone `Keystone` (and `Horizon`, if it pushes to OpenBao) CR.

A ControlPlane's fernet and credential keys are **irreplaceable**: the credential
keys decrypt every application credential, EC2 credential, and TOTP secret, and
their OpenBao backup is bound to a PushSecret that deletes the remote copy when
the binding goes away. Switching stores **moves** this material — it never
re-creates it: the operator updates the backup PushSecrets **in place**
(unchanged name and OpenBao path) so a store switch only re-points the identity.

The per-ControlPlane database and cache need no additional work: the
infrastructure a ControlPlane owns is already created in the ControlPlane's own
namespace, so two ControlPlanes already get two instances.

---

## Further reading

- [ControlPlane Quick Start](../quick-start-controlplane.md) — standing up a tenant as a `ControlPlane` CR (the one-per-namespace tenancy aggregate).
- [Enable the Keystone Operator NetworkPolicy](./enable-keystone-operator-networkpolicy.md) — confine the namespace-scoped operator's egress.
- [Helm Values Schema](../reference/backend/helm-values-schema.md) — the full `rbac.*` / `webhook.*` value reference.

## Tested by

The namespace-scoped install and the two-ControlPlanes-in-two-namespaces tenancy
this guide describes are asserted on the CI e2e kind cluster by these chainsaw
suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/namespace-scoped-rbac
chainsaw test --test-dir tests/e2e/c5c3/multi-controlplane
```
