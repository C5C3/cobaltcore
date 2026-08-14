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

Registering a cluster is a commitment to let every service operator cache it in
full. Each one watches, on every registered cluster, the kinds it projects there
and the inputs it reads (both enumerated in the next section), so its
credentials there need cluster-wide `list` and `watch` on all of them —
including `secrets`, `roles` and `rolebindings`. The operator install that
engages target clusters is cluster-scoped (see the next section), so those
informers are not restricted to a namespace: each operator process holds every
object of every watched kind, in every namespace of every registered cluster,
resident for its lifetime. Two consequences are worth weighing before a cluster
is registered rather than after:

- Anyone who obtains the operator's target-cluster credentials can read every
  Secret in every namespace of that cluster, and a heap dump or an exec into one
  operator pod yields the same material.
- The memory the operator needs scales with the whole fleet, not with the
  namespaces it projects into.

Restricting those caches to the namespaces that actually hold projecting CRs is
#841. Until it lands, do not register a target cluster you are not willing to
grant fleet-wide read on and to cache in full.

## Prerequisites on the management cluster

Target clusters need a cluster-scoped operator install. The registration Secrets
live outside the operator's own namespace, and a namespace-scoped install
(`rbac.namespaceScoped: true`) renders a Role in the release namespace and
nothing anywhere else. Its chart therefore passes `--clusters-namespace=`,
switching target clusters off: the operator engages no cluster and reads no
Secret it has no grant for. Left on, the widened Secret informer would fail to
sync and the manager would not start.

Every service operator watches, on each registered cluster, the kinds it
projects there and the inputs it reads: the credential Secrets, the database CR
for the services that have one, the OpenBao instance a Barbican secret store
waits on, and the ESO SecretStores and ClusterSecretStores. A child event maps
back to the owning CR through the ownership labels, an input event through the
same mappers the management-side watches use. A Deployment deleted on a target,
or a database password rotated there, is corrected within watch latency,
whatever cluster the infrastructure runs on. Engagement is provider-level: a
registered cluster serves one informer per watched kind to each service
operator, whether or not a CR names it — see the target-cluster prerequisites
above for what that costs. A watch is set up on a cluster only when that cluster
serves the kind, and the check runs once, as the cluster is engaged. A CRD
installed on a target after that is watched from the next engagement on, after a
registration-Secret rotation or an operator restart.

A child is matched to its owner only in the namespace that owner lives in, which
is the namespace its children land in. The ownership labels are readable and
writable by anyone with access to the target namespace, so an object carrying
them anywhere else is not treated as a child at all.

Nor is an event on a cluster the CR does not name. Because a leg is engaged on
every registered cluster, an event only reaches a CR if that CR's
`targetClusterRef` names the cluster the event came from — read from the CR on
the management cluster, never from the object that raised the event. Both the
child watches and the input watches apply it: without it, anyone able to create
an object in one shared namespace on any registered cluster could name any CR in
the fleet and have the operator reconcile it on their timing.

The watches against the management cluster register at builder time, so a
controller whose children all live on a target still needs the child kinds
installed at home. The three third-party CRD sets are therefore required on the
management cluster whatever `targetClusterRef` says.

| CRD set | Installed in this repo from |
| --- | --- |
| mariadb-operator | The `mariadb-operator-crds` chart on `https://mariadb-operator.github.io/mariadb-operator` |
| external-secrets | The `external-secrets` chart on `https://charts.external-secrets.io`, which bundles its CRDs |
| openbao-operator | The digest-pinned chart artifact `oci://ghcr.io/dc-tec/charts/openbao-operator` |

See [Infrastructure Manifests](./infrastructure/infrastructure-manifests.md) for
the Flux sources and the dependency order these three ride in.

## Ownership and teardown on the target

A child written to a target cluster carries no owner references at all. Nothing
on that cluster can resolve an owner into the management cluster, so ownership
is recorded in three labels the operator stamps on every remote child:

| Label | Value |
| --- | --- |
| `openstack.c5c3.io/owner-kind` | The owning CR's kind: `Keystone`, `Barbican`, `Horizon`, `Glance`, or `Placement` |
| `openstack.c5c3.io/owner-name` | The owning CR's name |
| `openstack.c5c3.io/owner-namespace` | The owning CR's namespace, which is the namespace the child lands in |

The kind is part of the key because a Keystone and a Barbican of the same name
in the same namespace project into one target namespace, and each has to select
only its own. Installing the service CRDs on a target cluster is safe: no child
there points at an owner, so no garbage collector has one to resolve.

A label value may not exceed 63 characters, so the name of a CR that names a
target cluster may not either. A longer one is refused at the first child write,
before anything is created.

::: warning One target namespace belongs to one management cluster
The three labels name the owning CR and say nothing about where it lives. Two
management clusters that each run these operators, each hold a CR of the same
kind and name in the same namespace, and each name the same target cluster would
project into that namespace under one identity: either would take the other's
children for its own, overwrite their specs, and delete them when its own CR is
deleted. Give each management cluster its own namespace on a shared target, or
its own target cluster.
:::

Deleting a CR that names a target cluster tears its children down explicitly.
The finalizer `openstack.c5c3.io/remote-children` goes on whenever
`targetClusterRef` is set, and holds the CR in etcd until the sweep has run. The
sweep deletes every object of that operator's projected kinds the CR owns, in the
CR's namespace on the target — by the three labels, or by a controller owner
reference an older operator left on it. It runs after the cleanup
flows that delete objects by name, the MariaDB CRs and, for Keystone, the backup
PushSecrets. That PushSecret flow holds the CR across passes until ESO has
purged the kv-v2 paths, and a sweep running first would delete the PushSecrets
out from under it. The order also matches the local one, where the cascade
starts only once every finalizer has released.

The sweep does not wait for the deleted objects to leave etcd. A child holding a
finalizer of its own, the MariaDB CRs being the slow ones, finishes terminating
on the target after the CR is gone, the way it does under the local cascade.

Horizon needs no finalizer for a local deletion, since the cascade reclaims
every child it composes. A Horizon that names a target cluster carries the
remote-children finalizer and the same sweep.

A `BarbicanSecretStore` whose parent Barbican resolves to a target cluster
carries that finalizer too, plus the annotation
`openstack.c5c3.io/children-cluster` naming the cluster. Both are written before
the first credentials write. Its deletion deletes the AppRole credentials Secret
on the cluster its parent names and on the annotated one — the same cluster on
every ordinary teardown — so neither a parent deleted first nor a parent
re-created against a different target strands it. Only a Secret carrying this
store's ownership labels is taken. The AppRole on the OpenBao instance stays: it
is shared instance state, owned by the self-init contract rather than by one
store.

A cluster deregistered under a CR that is already being deleted holds the
deletion open. The pass requeues and the finalizer stays on until the two
five-minute windows run out. Then it is released without a sweep, under a
`RemoteChildrenAbandoned` warning event naming what went undeleted, and the
children stay on the unreachable cluster, to be removed there by hand.

::: warning Children written once keep an owner reference from an older operator
A child written by an operator predating this contract carries an owner
reference to the management-cluster CR. Children on the Server-Side Apply and
`CreateOrUpdate` paths shed it on the operator's first pass over them. Children
created once and never rewritten do not: the immutable config ConfigMaps and
Secrets, the derived db-connection Secret, the fernet and credential key
Secrets, the Certificates, and completed Jobs keep the stale reference for as
long as they exist. The sweep still reaches them — it recognizes the reference as
readily as the labels — so deleting the CR removes them. Until it is deleted the
reference is a hazard on a target cluster that has the service CRDs installed:
that cluster's garbage collector resolves it to a missing object and collects the
child as an orphan. Delete such a CR and create it anew before the CRDs go onto
its target cluster; everything it writes afterwards carries the labels alone.
:::

## Per-cluster capabilities

Two of the kinds these operators project are optional: the Gateway API
`HTTPRoute`, and the cert-manager `Certificate` Keystone issues for a managed
database client keypair. Whether a kind can be written is a property of the
cluster the children land on, so that is the cluster asked. A CR without
`targetClusterRef` takes the answer from the latch its operator probed against
the management cluster's `RESTMapper` at setup, and a CRD installed there
afterwards is picked up on the next operator restart. A CR that names a target
cluster is probed against that cluster's `RESTMapper` on every pass, with
nothing memoized in between. Install Gateway API on the target and the next
reconcile writes the route.

The verdict decides what the pass does. A `spec.gateway` set against a cluster
that does not serve `HTTPRoute` holds `HTTPRouteReady=False` with reason
`GatewayAPINotInstalled`, under a message naming the cluster that lacks the
CRD. Keystone's Certificate delete, the one that runs when database TLS is
switched off or pointed at a brownfield database, is skipped on a target
without cert-manager, where no Certificate can exist. A probe that fails
instead of answering is its own outcome: a target API server that is
unreachable, or throttling the discovery request, sets the sub-reconciler's own
condition — `HTTPRouteReady` or `DatabaseTLSReady` — to `False` with reason
`CapabilityProbeFailed`, and the pass is retried with backoff. That condition is
what keeps an aborted pass honest, since the aggregate `Ready` is re-computed
and `status.observedGeneration` stamped on every exit path.

A watch leg is fixed when its cluster is engaged (see above); the probe is not.
A CRD installed on a target after that takes effect on the next reconcile,
while the drift watch for that kind stays absent until the registration Secret
is rotated: an `HTTPRoute` deleted by hand on that cluster is corrected on the
next periodic requeue instead of within watch latency.

Field indexes are registered on the management cluster only. Every index is on
a CR kind, and the CRs exist there alone; a remote event finds its CR through
the local cache, because the mappers pin every request they emit to the
management cluster. Registering the indexes on the fleet would break cluster
engagement outright, since the kubeconfig provider applies its stored indexes
while engaging a cluster.

The API Service selector latch reads the target too: it goes through that
cluster's own uncached API reader, so a lagging cache cannot re-widen the
selector.

## Interim constraints

- `KeystoneIdentityBackend` carries no `targetClusterRef`, and its reconciler stays management-side. A backend attached to a Keystone that names a target cluster looks for the parent's Deployment and projection Secret locally, where they do not exist, and holds `ConfigProjected=False` with reason `WaitingForProjection`.
- RBAC and access packaging for a target cluster, together with the two-cluster development flow, are #841.
