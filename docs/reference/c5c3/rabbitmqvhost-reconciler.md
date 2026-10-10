---
title: RabbitMQVhost Reconciler Architecture
quadrant: operator
---

# RabbitMQVhost Reconciler Architecture

Reference documentation for the `RabbitMQVhostReconciler`. It owns the
[RabbitMQVhost](./rabbitmqvhost-crd.md) lifecycle and is the single writer of
its status: the teardown finalizer, the Vhost CR and the user generations on
the managed bus, the OpenBao backup, the Secret delivered beside the order, and
the `VhostReady` / `DeliveryReady` / `Ready` conditions.

It serves orders on the management cluster and on target clusters the way the
[KeystoneUser reconciler](./keystoneuser-reconciler.md) does: a request carries
the cluster the order was seen on, and the reconciler reads and writes the
order, its status, its finalizer and its delivered Secret through that
cluster's client. Everything else it creates lives in the ControlPlane's
namespace on the management cluster, beside the `RabbitmqCluster`.

It adds no broker client and no OpenBao client. The RabbitMQ Messaging Topology
Operator creates the vhost, the users and the permissions on the broker from
the `Vhost`, `User` and `Permission` CRs the reconciler writes, with the admin
credential the `RabbitmqCluster` reports. ESO pushes the credentials to OpenBao
through the secret store of the ControlPlane's namespace. The reconciler reads
nothing from Keystone, so it has no admin-credential gate, and it records no
Events, for the reason the KeystoneUser reconciler gives.

For the field-level contracts see
[RabbitMQVhost CRD API Reference](./rabbitmqvhost-crd.md).

## Controller Registration

```go
if err := (&controller.RabbitMQVhostReconciler{
    Client:                  mgr.GetClient(),
    Scheme:                  mgr.GetScheme(),
    Resolver:                mcMgr,
    MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
}).SetupWithManager(mcMgr); err != nil { ... }
```

`Client` is the management cluster's client and `Resolver` the multicluster
manager, as for the Keystone order kinds. `Now` is the clock the schedule
reads; it is nil in production, which means `time.Now`, and the unit tests set
it. `SetupWithManager` delegates to `setupWithOptions`, so the integration
suites register the same watches with `SkipNameValidation`.

The cluster-agnostic machinery (child naming and ownership, the admission
gates, the pass result, the teardown and the label mapping) is the shared
scaffold in `keystoneorder.go`. See
[The shared scaffold](./keystone-orders-reconciler.md#the-shared-scaffold). The
three topology kinds are addressed unstructured through
`internal/common/messaging/topology.go`, as the `RabbitmqCluster` is, so the
operator takes no Go-module dependency on the topology operator.

Before the watches it registers one field indexer, on the local manager's
indexer only, for the reason the KeystoneUser reconciler gives:

| Index | Key | Extractor |
| --- | --- | --- |
| `RabbitMQVhostControlPlaneRefIndexKey` (`spec.controlPlaneRef`) | the resolved `<namespace>/<name>` of the reference | `rabbitMQVhostControlPlaneRefExtractor` |

### Watches

| Resource | Watch | Clusters | Effect |
| --- | --- | --- | --- |
| `RabbitMQVhost` | `For()` | management, and every engaged target cluster that serves the kind | Filtered by `watch.CRUpdatePredicate()`. The request carries the event's cluster |
| `Secret` | `Owns()` | the same | The delivered Secret beside the order |
| `Secret`, `PushSecret` | `Watches()` | management | Mapped back to the order by the three ownership labels (`orderChildRequests`). The cluster label becomes the request's cluster |
| `Vhost`, `User`, `Permission` (`rabbitmq.com/v1beta1`) | `Watches()` | management | The same mapping, registered for each kind the discovery probe finds served. The topology operator reporting a child Ready, or letting go of a deleted one, brings the order back |
| `ControlPlane` | `Watches()` | management | Index-backed fan-out (`controlPlaneToRabbitMQVhostsMapper`), filtered by `orderControlPlanePredicate()` |

The topology operator is optional infrastructure a Keystone-only install never
carries. Its three kinds are listed in `optionalWatchObjects`, and
`setupWithOptions` probes discovery (`probeOptionalWatches`) and skips the watch
of a kind the management cluster does not serve, so the operator starts on such
a cluster. The ControlPlane controller registers the `crdWatchGate` over the
same list, which restarts the process once a missing kind appears. See
[the presence probe](./controlplane-reconciler.md).

No ControlPlane watch reaches an order on a target cluster. Such an order is
reconciled again every 10 minutes (`orderRefreshAfter`), which is how a
published messaging endpoint reaches it.

### RBAC

On the management cluster the ClusterRole gains the order verbs and the write
verbs on the three topology kinds:

```go
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=rabbitmq.com,resources=vhosts;users;permissions,verbs=get;list;watch;create;update;patch;delete
```

It never creates or deletes an order. The Secrets and PushSecrets it writes in
the ControlPlane's namespace, and its reads of the ControlPlane and the
`RabbitmqCluster`, are granted by the ControlPlane's marker block. It never
reads the broker's admin credential beyond the default-user Secret the
ControlPlane's bus resolution reads already.

On a target cluster the `target-cluster-access` chart grants the same order
verbs and Secret write in each assigned namespace, and read on the order kind
in each placed namespace. See
[Assigned namespaces](../target-clusters.md#assigned-namespaces).

## Reconciliation Flow

```
Reconcile
  ├─ resolve the order's cluster ──→ unresolvable: log, no requeue
  ├─ deleting? ───────────────────→ reconcileDelete  (see Deletion and Teardown)
  └─ reconcileNormal
       ├─ orderAdmission
       │    ├─ cluster name ≤ 63   → ClusterNameTooLong
       │    ├─ ControlPlane read   → ControlPlaneNotFound (10s); on a target
       │    │                        cluster NamespaceNotAssigned (1m)
       │    └─ namespace assignment → NamespaceNotAssigned (1m), frozen
       ├─ EnsureFinalizer          c5c3.io/rabbitmqvhost-teardown
       ├─ rabbitMQVhostMessagingGate
       │    ├─ no messaging block   → MessagingNotDeclared
       │    ├─ brownfield secretRef → MessagingNotManaged
       │    ├─ default user missing → WaitingForMessagingCredentials (10s)
       │    └─ not AllReplicasReady → WaitingForMessaging (10s)
       ├─ provisionVhost      (instrumented: RabbitMQVhostProvision)
       └─ deliverCredentials  (instrumented: RabbitMQVhostDelivery)
```

The admission gates and the freeze are the scaffold's, as for every order
kind. The messaging gate writes both sub-conditions. A refusal only a
ControlPlane edit lifts asks for no requeue of its own; the pass result turns
it into the 10-minute refresh. The gate reads the broker's in-cluster host and
port off the default-user Secret the `RabbitmqCluster` names
(`messaging.ResolveTransportURL`); no condition ever quotes the URL, because it
carries the admin password.

When the provision step has no live user yet, `DeliveryReady` reads
`WaitingForPassword` and the delivery is skipped. The pass result is the
shorter of the legs' requeue, which carries the rotation timers, and the
scaffold's: a converged order on the management cluster waits for its timer or
an event, and every other order comes back after 10 minutes at the latest.

## Vhost provisioning

`provisionVhost` writes `VhostReady`:

1. The secret store of the ControlPlane's namespace has to be ready, or the
   password could not be backed up (`SecretStoreNotReady`).
2. The Vhost CR `<prefix>vhost` is applied on every pass, with `spec.name` the
   derived vhost, `spec.deletionPolicy` the order's policy in the topology
   operator's spelling (`retain`, `delete`) and the `RabbitmqCluster` as
   `rabbitmqClusterReference`, so a changed policy reaches it. A cluster that
   serves no Vhost kind reads `TopologyOperatorNotInstalled` and comes back
   after 10 minutes. A Vhost of that name the order did not create is refused
   with `VhostError` and no requeue. The topology operator's
   `FailedCreateOrUpdate` reads `VhostFailed` with its message; until it
   reports the Vhost Ready at its current generation the step reads
   `WaitingForVhost`.
3. The order's Users, Permissions and password Secrets are listed by its labels
   and keyed by generation. `vhostDesiredGeneration` decides the generation the
   pass works toward: the declared one before the first user is live, a User
   above the live generation as a rotation in flight, or a new rotation when
   the schedule is due, a higher generation is declared, or the live
   generation lost its password, its User or its Permission. No rotation starts
   while a grace period runs. Every generation that is neither live, wanted,
   nor in its grace period is deleted.
4. To create generation N, the password Secret `<prefix>password-v<N>` is
   written with `username` `<vhost>-v<N>`, a 43-character `password` generated
   once, and the label `rabbitmq.com/topology-operator: "true"`, which the
   topology operator's webhook requires on an imported Secret. The User
   `<prefix>user-v<N>` imports it, and the Permission `<prefix>permission-v<N>`
   grants `.*` three times on the vhost once the User is Ready: the broker
   refuses permissions for a user it does not hold yet. While either is not
   Ready the first generation reads `WaitingForUser`; a later one keeps
   `VhostReady` at `VhostProvisioned`, because the delivered user stays valid.
   A broker refusal reads `VhostFailed`. Once both are Ready status switches:
   `passwordGeneration`, `username` and `lastRotation`, and for a rotation
   `previousPasswordGeneration` and `previousPasswordDeleteAt`.
5. Once `previousPasswordDeleteAt` has passed, the superseded Permission and
   User are deleted. On the pass that no longer lists the User, its password
   Secret goes and the two `previous*` fields are cleared.
6. `status.nextRotation` is `lastRotation` plus the interval, or absent with
   the schedule off, and the step asks to come back at the earlier of it and
   the end of the grace period.

A converged pass reads the ControlPlane, the `RabbitmqCluster` and its
default-user Secret, applies the Vhost, and lists the Users, the Permissions
and the Secrets. The topology kinds are read from the API server directly,
since the operator's client caches no unstructured kind.

## Credential delivery

`deliverCredentials` writes `DeliveryReady`:

1. The broker address is resolved for the order's cluster
   (`rabbitMQVhostEndpoint`): the in-cluster host and port on the management
   cluster, the ControlPlane's `publishedMessagingEndpoint` elsewhere. Without
   one the step reports `MessagingNotPublished` and writes nothing; a
   published value without a port between 1 and 65535 reads `DeliveryError`.
2. The password of the live generation is read from `<prefix>password-v<N>`
   (`WaitingForPassword` while it is not there).
3. The transport URL `rabbit://<user>:<password>@<host>:<port>/<vhost>` is
   built (`messaging.BuildTransportURLForVhost`), and the source Secret
   `<prefix>source` gets it with the five other keys.
4. The PushSecret `<prefix>backup` pushes the source to
   `openstack/rabbitmq/<controlplane namespace>/<prefix without the trailing dash>/credentials`
   through the namespace's own store, with `deletionPolicy: Delete`. A changed
   transport URL sets a hash annotation on it and records the PushSecret's
   `status.syncedResourceVersion`, so the step waits for a push of the current
   credentials (`BackupNotSynced`), as the KeystoneUser delivery does.
5. The Secret `<name>-credentials` is applied beside the order with a
   controller owner reference to it. A live Secret of that name the order does
   not own is refused (`DeliveryRefused`), read from the order cluster's API
   server.
6. Once the apply response carries the current password, `status.host`,
   `status.port`, `status.secretName` and `status.secretKeys` are set and
   `DeliveryReady` reads `Delivered`.

The apply in step 5 is the repair: Server-Side Apply with forced ownership
gives the operator's field manager the six keys it applies.

## Child Naming and Placement

| Object | Name | Namespace | Cluster | Ownership |
| --- | --- | --- | --- | --- |
| Vhost CR | `<prefix>vhost` | ControlPlane's | management | labels |
| Password Secret, per generation | `<prefix>password-v<N>` | ControlPlane's | management | labels |
| User CR, per generation | `<prefix>user-v<N>` | ControlPlane's | management | labels |
| Permission CR, per generation | `<prefix>permission-v<N>` | ControlPlane's | management | labels |
| Source Secret | `<prefix>source` | ControlPlane's | management | labels |
| PushSecret | `<prefix>backup` | ControlPlane's | management | labels |
| Delivered Secret | `<name>-credentials` | the order's | the order's | controller owner reference |

`<prefix>` is `<name>-<first 8 hex of sha256(<cluster>/<namespace>/<name>)>-vhost-`,
and the vhost on the broker is the prefix without its `-vhost-` tail. The
labels are `c5c3.io/rabbitmqvhost-name`, `c5c3.io/rabbitmqvhost-namespace` and
`c5c3.io/rabbitmqvhost-cluster`, the last empty for the management cluster. An
object in the ControlPlane's namespace is the order's child only when it carries
all three labels and the prefix.

## Deletion and Teardown

`reconcileDelete` runs `orderTeardown` with the
`c5c3.io/rabbitmqvhost-teardown` finalizer, without consulting the assignment,
so a frozen order tears down too. Nothing references a RabbitMQVhost, so there
is no hold. `sweepRabbitMQVhostChildren` issues the deletes in this order, in
the resolved `controlPlaneRef` namespace:

1. the PushSecret, whose `deletionPolicy` has ESO remove the OpenBao path,
2. every Permission,
3. every User, whose topology-operator finalizer removes the user from the
   broker,
4. the Vhost, after a merge patch has written the order's current
   `deletionPolicy` onto it (a Vhost already being deleted keeps its own); the
   topology operator removes the vhost from the broker under `delete` and
   leaves it under `retain`,
5. every password Secret, then the source Secret,

and then the delivered Secret through the order's cluster client. Each
topology kind is one list; a cluster that serves none of a kind holds none of
it. With the ControlPlane present the finalizer is held while any child is
still listed, and the pass requeues after 10 seconds. With the ControlPlane
gone the deletes are issued and the finalizer is released at once; the
topology operator removes a CR whose `RabbitmqCluster` is gone without reaching
the broker.

## Metrics Instrumentation

Both steps run through the package-scope `instrumenter`, as for the Keystone
order kinds:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `RabbitMQVhostProvision` | `VhostReady` |
| `RabbitMQVhostDelivery` | `DeliveryReady` |

`TestSubReconcilerConditionTypes_RabbitMQVhostLegs` pins the two entries, and
the drift guard `TestSubReconcilerConditionTypesCoversAllNames` accepts a value
from `rabbitMQVhostSubConditionTypes`.

## Testing

| Layer | Location |
| --- | --- |
| Gates, freeze, mappers, teardown | `operators/c5c3/internal/controller/rabbitmqvhost_controller_test.go` |
| Vhost provisioning and rotation | `operators/c5c3/internal/controller/rabbitmqvhost_provision_test.go` |
| Credential delivery | `operators/c5c3/internal/controller/rabbitmqvhost_delivery_test.go` |
| The topology builders and readers, the vhost transport URL | `internal/common/messaging/topology_test.go`, `messaging_test.go` |
| CRD schema | `TestIntegration_RabbitMQVhost_SchemaValidation` in `integration_test.go` |
| An order on a target cluster | `TestIntegration_Multicluster_ControlPlanePlacement` in `multicluster_integration_test.go` |
| Startup without the topology kinds | `TestRabbitMQVhostSetup_StartsWithoutTopologyCRDs` in `setupwithmanager_integration_test.go` |
| End to end | `tests/e2e/c5c3/messaging/` |
| Admission rejection corpus | `tests/e2e/c5c3/invalid-rabbitmqvhost-cr/` |

The unit tests drive a fake client per cluster and report the topology
children Ready by hand. The envtest suites install the three topology kinds as
schema without a controller, except the startup test, which leaves them out.
The end-to-end suite is where the order meets the topology operator, the
broker, OpenBao and ESO. See
[ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
