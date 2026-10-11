---
title: RabbitMQVhost CRD API Reference
quadrant: operator
---

# RabbitMQVhost CRD API Reference

Reference documentation for the RabbitMQVhost Custom Resource Definition. A
RabbitMQVhost orders one vhost on the managed message bus of a
[ControlPlane](./controlplane-crd.md), with one user that holds configure,
write and read permissions of `.*` on it, from a namespace the ControlPlane
assigns to a service owner. The c5c3-operator creates the vhost, the user and
the permission through the RabbitMQ Messaging Topology Operator, backs the
credentials up to OpenBao, and writes them into the Secret
`<metadata.name>-credentials` beside the order. On a schedule it creates a
successor user, switches the Secret to it, and deletes the superseded user
after a grace period.

The order lives in the assigned namespace, on the cluster that namespace is
assigned on: the management cluster or a registered
[target cluster](../target-clusters.md). Its children live in the
ControlPlane's namespace on the management cluster, beside the
`RabbitmqCluster` the ControlPlane declares.

For the control loop see
[RabbitMQVhost Reconciler Architecture](./rabbitmqvhost-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md#order-a-message-bus-vhost)
guide walks the flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `RabbitMQVhost` |
| List Kind | `RabbitMQVhostList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in `rabbitmqvhost_types.go`
registers both Kinds with the shared `SchemeBuilder`.

`kubectl get rabbitmqvhost` prints six columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Vhost` | `.status.vhost` |
| `Generation` | `.status.passwordGeneration` |
| `NextRotation` | `.status.nextRotation` |
| `Age` | `.metadata.creationTimestamp` |

## Example

A vhost for the assigned namespace `tenant-a`, on the bus of the ControlPlane
`cp` in `openstack`. The schedule rotates the user every three minutes and keeps
the superseded one for a minute, which is what the e2e suite uses; an order
without the block gets a new user every 720 hours and keeps the old one for 24.

<<< @/../tests/e2e/c5c3/messaging/01-rabbitmqvhost-tenant.yaml#rabbitmqvhost-workflow

The suite substitutes `@TENANT_NS@` and `@CP_NS@`; an order of your own names
its namespaces directly.

## Spec

### RabbitMQVhostSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | `ControlPlaneRefSpec` | Yes | none | The ControlPlane whose managed bus the vhost is created on. Both halves are **immutable**, as for a [KeystoneUser](./keystoneuser-crd.md#controlplanerefspec). |
| `passwordGeneration` | `int64` | No | `1` | The lowest password generation the order must hold (`Minimum=1`). Raising it above `status.passwordGeneration` rotates at once. It may only increase. |
| `rotation.interval` | `Duration` | No | `720h` | The time between two scheduled rotations. `0s` turns the schedule off; any other value is at least `1m`. At most `87600h`. |
| `rotation.gracePeriod` | `Duration` | No | `24h` | How long the superseded user stays on the broker after the delivered Secret switched. Not negative, and shorter than a non-zero interval. |
| `deletionPolicy` | `string` | No | `Retain` | What deleting the order does to the vhost: `Retain` keeps it with its queues and messages, `Delete` removes it. See [Deletion policy](#deletion-policy). |

The vhost name is not part of the spec. It is derived from the order as
`<metadata.name>-<8 hex>`, where the hash covers the order's cluster, namespace
and name. Two orders never share a vhost, so no order can reach another's
queues, and an order re-created with the same name reattaches to a vhost that a
`Retain` deletion kept. The user of password generation N is `<vhost>-v<N>`. It
holds configure, write and read `.*` on the vhost and nothing else, and carries
no management tags.

The API server stores the defaults it applies as written (`720h`, `24h`,
`Retain`). The kind has no webhook, so the reconciler resolves a zero
`passwordGeneration`, an absent duration and an empty `deletionPolicy` to their
defaults itself.

### Rotation

A rotation creates a new user, never a new password for the live one: a
consumer that still holds the old password keeps a valid login until it reads
the Secret again. Each rotation creates generation N+1 beside the delivered
generation N:

1. The operator writes the password Secret of generation N+1, creates the
   broker user `<vhost>-v<N+1>` from it, and grants it the permission once the
   broker holds the user. The delivered Secret keeps generation N, which stays
   valid, and `VhostReady` stays `True`.
2. Once the topology operator reports the successor's User and Permission
   Ready, the order switches to it: `status.passwordGeneration` reads N+1,
   `status.previousPasswordGeneration` names N, and the delivered Secret is
   rewritten as soon as the backup holds the new credentials.
3. When `status.previousPasswordDeleteAt` has passed, generation N's
   Permission and User are deleted, and the topology operator removes the user
   from the broker.

A rotation starts when one of these holds:

- the schedule is due: the clock has reached `status.nextRotation`, which is
  `status.lastRotation` plus the interval;
- `spec.passwordGeneration` is above `status.passwordGeneration`, which creates
  that generation directly;
- a piece of the live generation is gone: its password Secret lost the
  password, or its User or Permission was deleted. The broker user cannot be
  trusted to match the delivered Secret any more, so a new one replaces it.

No rotation starts while a grace period runs, so the order never holds more
than two users. A rotation already under way is continued. A successor the
broker refuses (`VhostFailed`) is continued as well until
`spec.passwordGeneration` is raised above it; the order then deletes it and
creates the raised generation. A refused successor reads `VhostReady=False`
while the delivered user stays valid.

### Deletion policy

`spec.deletionPolicy` is projected onto the Vhost CR as the topology operator's
own `deletionPolicy`, `retain` or `delete`, and may change at any time; the next
pass applies it. The teardown writes the order's current policy onto the Vhost
CR before it deletes it, so a policy changed right before the delete, or while
the order is frozen or waiting on a gate, still decides what happens to the
vhost.

| Policy | On deleting the order |
| --- | --- |
| `Retain` (default) | The vhost stays on the broker with its queues and messages. The user, its permission, the password Secrets and the OpenBao path go. |
| `Delete` | The vhost goes too, and every queue and message in it. |

A retained vhost is not swept later. It is the platform operator's to remove
with `rabbitmqctl delete_vhost <vhost>` in the broker pod, or it goes with the
ControlPlane's broker.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the only consent a RabbitMQVhost reads, as for a
[KeystoneUser](./keystoneuser-crd.md#consent). The entry's `allowedRoles` is not
consulted.

Without an entry both conditions read `False/NamespaceNotAssigned` and the
order is frozen: nothing is provisioned, rotated, delivered, repaired or swept,
and the schedule pauses. The vhost, the users and the delivered Secret it
already has stay. Withdrawing an entry is therefore not a revocation; deleting
the order is.

Only a managed bus serves the order. A ControlPlane without
`spec.infrastructure.messaging` refuses it with `MessagingNotDeclared`. One that
attaches to a brownfield broker through `spec.infrastructure.messaging.secretRef`
refuses it with `MessagingNotManaged`: the operator knows such a broker by its
transport URL alone and holds no admin account on it.

The topology operator has to run on the management cluster. Without its
`Vhost` kind the order reads `VhostReady=False/TopologyOperatorNotInstalled`,
naming `deploy/flux-system/releases/messaging-topology-operator.yaml`, and is
retried every 10 minutes.

### Orders on a target cluster

An order for a namespace assigned on a target cluster is created on that
cluster. The operator watches the kind there, writes the order's status and
finalizer there, and writes the delivered Secret beside it. The Vhost, User and
Permission CRs, the password Secrets and the backup stay on the management
cluster, where the broker runs.

The broker's Service resolves only on the management cluster. An order on a
target cluster is delivered the address the ControlPlane publishes in
[`spec.infrastructure.publishedMessagingEndpoint`](./controlplane-crd.md#infrastructurespec),
a `host:port` the platform operator exposes by their own means; the operator
publishes nothing itself. Without it the order reads
`DeliveryReady=False/MessagingNotPublished`, and the vhost and its user are
provisioned all the same.

The target cluster serves the kind through the `target-cluster-access` chart,
which ships the CRD and grants the operator's account the order and Secret
verbs in each `assignedNamespaces` entry. See
[Assigned namespaces](../target-clusters.md#assigned-namespaces). An order on a
cluster registered under a name longer than 63 characters reports
`ClusterNameTooLong` on both conditions.

## Delivered Secret contract

| Property | Value |
| --- | --- |
| Name | `<metadata.name>-credentials`, also reported in `status.secretName` |
| Namespace | the order's own |
| Cluster | the order's own |
| Data keys | `transport_url`, `host`, `port`, `username`, `password` and `vhost`, also reported in `status.secretKeys` |
| Ownership | a controller owner reference to the order; the garbage collector reaps the Secret with it |
| Labels | `c5c3.io/rabbitmqvhost-name` and `c5c3.io/rabbitmqvhost-namespace` |
| Backing path | `openstack/rabbitmq/<controlplane namespace>/<name>-<hash>-vhost/credentials` in OpenBao |

`transport_url` is the oslo.messaging URL
`rabbit://<username>:<password>@<host>:<port>/<vhost>`, with the password
percent-encoded where it needs to be. The other five keys carry its parts for
consumers that build their own configuration. `host` and `port` are the
broker's in-cluster address on the management cluster and the published
endpoint elsewhere, also reported in `status.host` and `status.port`. A managed
bus runs no TLS listener, so the Secret carries no CA key.

The Secret always carries the generation `status.passwordGeneration` names. It
switches to a successor only once the broker holds it, and the superseded user
stays valid for the grace period after the switch, so the delivered credential
is never invalid. A consumer reads the Secret again within the grace period to
pick the successor up.

The backup comes first, as for a KeystoneUser: the delivered Secret is written
once ESO reports a push of the current credentials to the backing path. The
operator applies the Secret with Server-Side Apply and owns its six keys. An
edited key is rewritten and a deleted Secret is written again; a key somebody
else adds stays. A [frozen](#consent) order repairs nothing.

A Secret of the same name that the order does not own is never taken over: the
order reports `DeliveryRefused`. A Keystone order of the same name in the same
namespace delivers a Secret of the same name, so the two orders cannot share a
name.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `VhostReady`, `DeliveryReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `vhost` | `string` | The vhost on the broker, `<metadata.name>-<8 hex>`. |
| `username` | `string` | The broker user the delivered Secret carries, `<vhost>-v<N>`. |
| `host` | `string` | The broker host the delivered Secret names. |
| `port` | `int32` | The broker port the delivered Secret names. |
| `passwordGeneration` | `int64` | The generation of the user the delivered Secret carries. |
| `lastRotation` | `*metav1.Time` | When that user became the delivered one. |
| `nextRotation` | `*metav1.Time` | When the schedule rotates next. Absent with the schedule off. |
| `previousPasswordGeneration` | `int64` | The generation of the superseded user, during its grace period only. |
| `previousPasswordDeleteAt` | `*metav1.Time` | When the superseded user is deleted, during its grace period only. |
| `secretName` | `string` | The [delivered Secret](#delivered-secret-contract), once it carries the current user. |
| `secretKeys` | `[]string` | `transport_url`, `host`, `port`, `username`, `password` and `vhost`, once the Secret carries the current user. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `VhostReady` | True | `VhostProvisioned` | The vhost and the delivered user exist on the broker. While a successor is created the message names it. |
| `VhostReady` | False | `SecretStoreNotReady` | The secret store of the ControlPlane's namespace is not ready, so the password could not be backed up. |
| `VhostReady` | False | `TopologyOperatorNotInstalled` | The management cluster serves no topology kind; the message names the release file that installs it. |
| `VhostReady` | False | `WaitingForVhost` | The topology operator has not reported the Vhost Ready yet. |
| `VhostReady` | False | `WaitingForUser` | The topology operator has not reported the first generation's User or Permission Ready yet. |
| `VhostReady` | False | `VhostFailed` | The broker refused the Vhost, a User or a Permission; the message carries the topology operator's own. |
| `VhostReady` | False | `VhostError` | A Kubernetes-level failure writing or reading the order's children, or a child of that name the order did not create. |
| `DeliveryReady` | True | `Delivered` | The Secret beside the order carries the current user. |
| `DeliveryReady` | False | `WaitingForPassword` | No user is provisioned yet, or the Secret does not carry the current password yet. |
| `DeliveryReady` | False | `MessagingNotPublished` | The order lives on another cluster than the broker and the ControlPlane publishes no messaging endpoint. |
| `DeliveryReady` | False | `BackupNotSynced` | ESO has not pushed the current credentials to OpenBao yet. |
| `DeliveryReady` | False | `DeliveryRefused` | A Secret of the delivered name exists that the order does not own. |
| `DeliveryReady` | False | `DeliveryError` | A Kubernetes-level failure writing or reading a delivery object, or a published endpoint without a valid port. |
| both | False | `MessagingNotDeclared` | The ControlPlane declares no `spec.infrastructure.messaging`. |
| both | False | `MessagingNotManaged` | The ControlPlane attaches to a brownfield broker through `secretRef`. |
| both | False | `WaitingForMessagingCredentials` | The `RabbitmqCluster` or its default-user Secret is not there yet. |
| both | False | `WaitingForMessaging` | The `RabbitmqCluster` is not `AllReplicasReady`. |
| both | False | `VhostError` | Reading the managed bus failed. |
| both | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| both | False | `NamespaceNotAssigned` | No `spec.namespaceAssignments` entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| both | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than the 63 characters a label value carries. |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

A changed `spec.deletionPolicy` reaches the Vhost CR on the next pass, and the
order reads `WaitingForVhost` for the pass in which the topology operator has
not caught up with the change.

A converged order on the management cluster is reconciled again at
`status.nextRotation`, at `status.previousPasswordDeleteAt`, and when its
ControlPlane or its children change. An order on a target cluster is reconciled
again every 10 minutes at the latest.

## Defaulting and Validation Summary

The kind has **no webhook**, because a target cluster runs none. Every rule is
a CRD rule:

- `passwordGeneration`: schema default `1`, `Minimum=1`, and the transition
  rule `self >= oldSelf` ("passwordGeneration may only increase").
- `rotation`: schema default `{}`, so the two defaults below apply to an order
  that omits the block.
- `rotation.interval`: schema default `720h`, the rule
  `duration(self) == duration('0s') || duration(self) >= duration('1m')`
  ("rotation.interval is 0s or at least 1m"), and the rule
  `duration(self) <= duration('87600h')` ("rotation.interval is at most
  87600h").
- `rotation.gracePeriod`: schema default `24h` and the rule
  `duration(self) >= duration('0s')` ("rotation.gracePeriod is not negative").
- On the `rotation` block: the grace period is shorter than a non-zero interval
  ("rotation.gracePeriod must be shorter than rotation.interval").
- `deletionPolicy`: schema default `Retain` and `Enum=Retain;Delete`.
- `controlPlaneRef`: as for a KeystoneUser ("controlPlaneRef.name is
  immutable", "controlPlaneRef.namespace is immutable").
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes").

Cross-object rules live in the reconciler: the consent and the managed bus each
need a read admission does not have.

The rejection corpus lives in `tests/e2e/c5c3/invalid-rabbitmqvhost-cr/`,
generated from its `_generate.py` and guarded by
`make verify-invalid-cr-fixtures`.

## Projected child names

The children live in the **ControlPlane's** namespace on the management
cluster, named from a per-order prefix:

```
<metadata.name>-<8 hex>-vhost-<discriminator>
```

The hash covers `<cluster>/<namespace>/<name>`, as for every order kind, and
the vhost on the broker is the prefix without its `-vhost-` tail. The
discriminators are:

| Discriminator | Child |
| --- | --- |
| `vhost` | The topology operator's `Vhost` CR. |
| `password-v<N>` | The username and password of generation N, generated once and labelled `rabbitmq.com/topology-operator: "true"`, the label the topology operator reads imported credentials by. |
| `user-v<N>` | The `User` CR of generation N, importing its credentials from `password-v<N>`. |
| `permission-v<N>` | The `Permission` CR granting generation N's user `.*` on the vhost. |
| `source` | The credentials the backup pushes. |
| `backup` | The PushSecret. |

Each carries three labels: `c5c3.io/rabbitmqvhost-name`,
`c5c3.io/rabbitmqvhost-namespace` and `c5c3.io/rabbitmqvhost-cluster` (empty
for the management cluster). The [delivered Secret](#delivered-secret-contract)
has no prefix and an owner reference instead.

## Deletion Semantics

The `c5c3.io/rabbitmqvhost-teardown` finalizer is installed once the order's
namespace is assigned. Nothing references a RabbitMQVhost, so deleting one
never holds on another order. The teardown issues the deletes in this order, in
the ControlPlane's namespace:

1. the PushSecret, which removes the OpenBao path;
2. every Permission;
3. every User, which the topology operator removes from the broker;
4. the Vhost CR, once it carries the order's current policy, which the
   topology operator removes from the broker under `Delete` and leaves under
   `Retain`;
5. the password Secrets and the source Secret;

and then the delivered Secret beside the order. The finalizer is held while any
of them is still listed, which is until the topology operator's finalizers let
go. With the ControlPlane gone the deletes are issued and the finalizer is
released in one pass; the topology operator lets go of a CR whose
`RabbitmqCluster` is gone without reaching the broker. A Vhost CR the order's
policy cannot be written onto is not deleted: the other deletes still go ahead,
and with the ControlPlane gone that CR stays behind. Set its `deletionPolicy`
and delete it by hand.

An order whose target cluster was deregistered cannot be read any more, so its
children in the ControlPlane's namespace stay. Remove them by hand:

```bash
kubectl delete vhosts.rabbitmq.com,users.rabbitmq.com,permissions.rabbitmq.com,pushsecrets.external-secrets.io,secrets \
  -n <controlplane-namespace> \
  -l c5c3.io/rabbitmqvhost-cluster=<cluster>,c5c3.io/rabbitmqvhost-namespace=<namespace>,c5c3.io/rabbitmqvhost-name=<name>
```

## Upgrade note

The backup writes `openstack/rabbitmq/<controlplane namespace>/…`, a path the
`eso-tenant` OpenBao policy grants from this release on. An existing deployment
runs `deploy/openbao/bootstrap/setup-policies.sh` once more before its first
order; until it does, the PushSecret fails with 403 and the order holds at
`DeliveryReady=False/BackupNotSynced`. The topology operator arrives with the
Flux base (`deploy/flux-system/releases/messaging-topology-operator.yaml`). See
[OpenBao Bootstrap](../infrastructure/openbao-bootstrap.md) and
[RabbitMQ Messaging Topology Operator](../infrastructure/infrastructure-manifests.md#rabbitmq-messaging-topology-operator).

## Chainsaw E2E Tests

`tests/e2e/c5c3/messaging/` runs on the `e2e-operator (c5c3)` job, the one
suite with a live managed bus. It orders a vhost, checks the six keys, the
backup path and the user's permissions with `rabbitmqctl` in the broker pod,
repairs the Secret, observes the scheduled rotation with both users valid
during the grace period and the old one refused afterwards, rotates by hand
with the schedule off, deletes one order under `Retain` and one under `Delete`,
and freezes an order by withdrawing the assignment.
`tests/e2e/c5c3/invalid-rabbitmqvhost-cr/` is the admission rejection corpus,
run on the same job. The target-cluster path is proven in
`TestIntegration_Multicluster_ControlPlanePlacement`.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
