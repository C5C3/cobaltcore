---
title: Target Clusters
quadrant: operator
---

# Target Clusters

Five workload CRDs carry an optional `spec.targetClusterRef`:
[Keystone](./keystone/keystone-crd.md), [Barbican](./barbican/barbican-crd.md),
[Horizon](./horizon/horizon-crd.md), [Glance](./glance/glance-crd.md), and
[Placement](./placement/placement-crd.md). The field names a registered target
cluster that receives every child the CR projects: Deployments, ConfigMaps,
Secrets, and, for the services that have one, the database CRs. The CR itself
does not move. It is created, reconciled, and deleted on the management cluster,
and so are its status, its finalizers, and the webhooks that admit it.

Omitting the field selects the local cluster, the one the operator runs on. The
children are created there and the deployment behaves like a single-cluster one,
so an existing CR keeps its behavior without an edit.

## The field

`name` is the only key. It must be a non-empty DNS-1123 subdomain
(`MinLength=1` plus the Kubernetes object-name pattern) and it must match the
name of a registration Secret.

```yaml
spec:
  targetClusterRef:
    name: edge-1
```

The ref is immutable once the CR exists. Adding it, removing it, or renaming it
is rejected by two spec-level CEL transition rules with the message
`targetClusterRef is immutable`, and again by the validating webhook, which
appends the reason to that same text. The rules are evaluated only on UPDATE, and
being schema-level they hold when the webhook is down. Each of those edits would
strand the children already created on the previously selected cluster. Moving a
service between clusters means deleting the CR and creating it anew.

## Registering a target cluster

A target cluster is registered by a kubeconfig Secret on the management cluster.
The Secret's name is the cluster name a CR references.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: edge-1
  namespace: c5c3-clusters
  labels:
    sigs.k8s.io/multicluster-runtime-kubeconfig: "true"
type: Opaque
data:
  kubeconfig: <base64-encoded kubeconfig>
```

Three parts of that Secret are contractual:

| Part | Value |
| --- | --- |
| Namespace | The operator's `--clusters-namespace` flag, default `c5c3-clusters`. The Secret informer is widened to this one namespace, and clearing the flag switches target clusters off entirely: no cluster is engaged and no Secret is read outside the watched namespace |
| Label | `sigs.k8s.io/multicluster-runtime-kubeconfig`, with the string value `"true"`. The label being present is not enough; any other value leaves the Secret unregistered |
| Data key | `kubeconfig` |

Any number of clusters may be registered at the same time, and each CR resolves
its own by name. Writing a new kubeconfig into the Secret reconnects: the
provider hashes the kubeconfig, sees the hash change, drops the existing
connection, and engages a fresh one. Deleting the Secret deregisters the
cluster.

Engagement is asynchronous. A CR created moments before its cluster engages
reports `TargetClusterUnavailable` briefly and heals on the next requeue.

## Who may name a cluster

::: warning Registering a cluster hands it to every CR the operator watches
`spec.targetClusterRef` is validated for shape and immutability, and for nothing
else. Nothing binds a CR, its namespace, or its author to the set of clusters it
may name, so once `edge-1` is registered, anyone who can create one of the five
CRs in a watched namespace can direct the operator's stored credentials for
`edge-1` at that cluster — with an image and a configuration of their choosing.
Treat write access to the clusters namespace, and create access to the five CRDs
on the management cluster, as equally privileged until an authorization model
lands.
:::

Two grants therefore need locking down on the management cluster:

| Grant | Why |
| --- | --- |
| `create`/`update` on Secrets in `c5c3-clusters` | A labelled Secret here registers a cluster. Whoever can write one decides which clusters the operator holds credentials for |
| `create` on `keystones`, `barbicans`, `glances`, `horizons`, `placements` | A CR author picks the cluster its children land on, from every name registered |

An install that needs no target clusters carries neither exposure: a
namespace-scoped install clears `--clusters-namespace` (see below), the operator
engages nothing, and a `targetClusterRef` naming any cluster reports
`TargetClusterUnavailable`.

## Registration does not validate credentials

A kubeconfig that does not parse never engages, and the cluster name stays
unresolvable. A kubeconfig that parses but carries credentials the target
rejects engages silently: nothing logs in at registration time. The failure
surfaces on the first child write, as the target API server's own error.

```text
the server has asked for the client to provide credentials
```

A wrong token therefore shows up on the first CR that uses the cluster, not when
the Secret is applied.

## When the name does not resolve

An unregistered name, or one whose Secret was deleted while CRs still pointed at
it, surfaces on the CR's first gate condition:

| CR | Condition | Status | Reason | Message |
| --- | --- | --- | --- | --- |
| Keystone, Barbican, Horizon, Glance, Placement | `SecretsReady` | `False` | `TargetClusterUnavailable` | The resolver's error, `cluster not found` for a name that was never registered |
| BarbicanSecretStore, GlanceBackend | `CredentialsReady` | `False` | `TargetClusterUnavailable` | Same |

The pass ends there. The CR requeues after 15 seconds, on a flat poll rather than
a backoff, and nothing is created on any cluster. Resolution runs before any
finalizer is installed, so a CR naming an unresolvable cluster carries none:
nothing was created for it, and a finalizer would only block its deletion.

A cluster that is deregistered under a running CR flips the same condition, and
the children already written to it stay where they are. The reconciler never
reaches its sub-reconcilers without a client, so nothing deletes them.

Deleting such a CR still works, after a grace period. The deletion path resolves
ahead of everything else and tolerates a target that is gone, but not right
away: while the grace period runs, an unresolvable target only requeues the
pass, every 15 seconds, the finalizers stay on, and the CR reports
`SecretsReady=False` with reason `TargetClusterUnavailable` and a message naming
the cluster it is waiting for, so the hold is not just a log line.

Two five-minute windows have to run out before the operator gives up: five
minutes since the CR was marked for deletion, and five minutes since that
operator process first failed to resolve the cluster. Engaging a cluster is
asynchronous, and rotating a registration Secret makes the provider drop the
cluster and build it again, so a registered cluster is indistinguishable from a
deregistered one for a moment after an operator restart or a rotation. Measuring
only from the deletion timestamp would give up on the first pass after such an
event whenever the CR had already been terminating for longer — a blocked
cleanup makes that ordinary — and strand the children of a CR whose cluster is
in fact perfectly reachable. A restarted operator therefore starts its own
window over.

Once both windows are out, the operator releases the finalizers, emits a
`RemoteChildrenAbandoned` warning naming what it could not delete, and lets the
CR leave etcd. The MariaDB CRs and the workload on the deregistered cluster are
left behind — unreachable, so nothing else was ever possible — and have to be
removed on that cluster by hand.

`BarbicanSecretStore` and `GlanceBackend` carry no `targetClusterRef` of their
own. Each resolves the target of the parent named by `spec.barbicanRef` or
`spec.glanceRef`, so an attachment lands on the same cluster as its parent's
children. A parent that does not exist — a dangling ref, or a GitOps apply that
has not landed it yet — leaves the target unknown, and both hold their first
gate condition at `WaitingForParent` rather than falling back to the management
cluster.

## Prerequisites on the target cluster

The CR's namespace must already exist on the target. The operator does not
create it, and a child write into a missing namespace fails.

The Barbican secret store mints a token through the target's TokenRequest API,
so the operator's credentials there need the corresponding RBAC. Packaging the
access a target cluster has to grant is #841.

## Prerequisites on the management cluster

Target clusters need a cluster-scoped operator install. The registration Secrets
live outside the operator's own namespace, and a namespace-scoped install
(`rbac.namespaceScoped: true`) renders a Role in the release namespace and
nothing anywhere else. Its chart therefore passes `--clusters-namespace=`,
switching target clusters off: the operator engages no cluster and reads no
Secret it has no grant for. Left on, the widened Secret informer would fail to
sync and the manager would not start.

The operators' child watches stay local: each controller registers its watches
against the management-cluster cache, so a controller whose children all live on
a target still needs the child kinds installed at home. The three third-party
CRD sets are therefore required on the management cluster whatever
`targetClusterRef` says.

| CRD set | Installed in this repo from |
| --- | --- |
| mariadb-operator | The `mariadb-operator-crds` chart on `https://mariadb-operator.github.io/mariadb-operator` |
| external-secrets | The `external-secrets` chart on `https://charts.external-secrets.io`, which bundles its CRDs |
| openbao-operator | The digest-pinned chart artifact `oci://ghcr.io/dc-tec/charts/openbao-operator` |

See [Infrastructure Manifests](./infrastructure/infrastructure-manifests.md) for
the Flux sources and the dependency order these three ride in.

## Owner references dangle on the target

::: warning A target cluster must not have the service CRDs installed
A child written to a target carries an owner reference to a CR that lives on the
management cluster, so on the target that reference points at nothing. With the
service CRDs installed there, the target's garbage collector resolves the owner
as absent and deletes the children. Keep the service CRDs off target clusters
until #837 replaces the owner references with an explicit remote-cleanup path.
:::

The same dangling reference is what makes deletion incomplete. Deleting a CR
removes the MariaDB CRs its finalizer cleans up, and leaves everything else —
Deployment, Service, ConfigMaps, Secrets, HTTPRoute — running on the target,
where no garbage collector can resolve the owner that is gone. Horizon installs
no finalizer at all, so a deleted Horizon leaves its whole projection behind,
including the Secret carrying the Django `SECRET_KEY`. Delete the CR's namespace
on the target cluster, or the objects in it, to reclaim them.

## Interim constraints

- Drift on a target cluster is corrected on CR events and requeues only. The operators do not watch child objects on target clusters, so an edit made directly on the target survives until the next pass (#838).
- The capability probes (Gateway API, cert-manager) run against the management cluster, not the target (#839). The API Service selector latch does read the target: it goes through that cluster's own uncached API reader, so a lagging cache cannot re-widen the selector.
- `KeystoneIdentityBackend` carries no `targetClusterRef`, and its reconciler stays management-side. A backend attached to a Keystone that names a target cluster looks for the parent's Deployment and projection Secret locally, where they do not exist, and holds `ConfigProjected=False` with reason `WaitingForProjection`.
- RBAC and access packaging for a target cluster, together with the two-cluster development flow, are #841.
- Garbage-collection-safe ownership for remote children is #837.
