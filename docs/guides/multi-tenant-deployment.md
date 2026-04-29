---
title: Multi-Tenant Deployment
quadrant: operator
---

# Multi-Tenant Deployment

Guide for deploying the Keystone operator in namespace-scoped mode.
In this mode the operator uses a `Role` / `RoleBinding` instead of
`ClusterRole` / `ClusterRoleBinding` and restricts its cache, watches, and
reconciliation to a single namespace.

## Prerequisites

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
`autoscaling` metric requirements, and minimum-value checks.

---

## Example: namespace-scoped install

Deploy the operator into the `team-alpha` namespace with namespace-scoped RBAC:

```bash
helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --namespace team-alpha --create-namespace \
  --set rbac.namespaceScoped=true \
  --set webhook.enabled=false \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=v0.1.0 \
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
