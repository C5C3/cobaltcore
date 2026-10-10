---
title: MariaDBDatabase Reconciler Architecture
quadrant: operator
---

# MariaDBDatabase Reconciler Architecture

Reference documentation for the `MariaDBDatabaseReconciler`. It owns the
[MariaDBDatabase](./mariadbdatabase-crd.md) lifecycle and is the single writer
of its status: the teardown finalizer, the OpenBao database-engine role, the
mariadb-operator Database CR, the ESO generator and ExternalSecret that issue
the credential, the Secret delivered beside the order, and the
`DatabaseReady` / `DeliveryReady` / `Ready` conditions.

Like the [Keystone order reconcilers](./keystone-orders-reconciler.md) it serves
orders on the management cluster and on target clusters. A request carries the
cluster the order was seen on, and the reconciler reads and writes the order,
its status, its finalizer and its delivered Secret through that cluster's
client. The generator and the ExternalSecret live in the ControlPlane's
namespace on the management cluster, and the Database CR beside the MariaDB in
Keystone's namespace on Keystone's cluster.

It is the one reconciler in the c5c3-operator with an OpenBao client of its own
(`internal/common/openbao`). It logs in on the `kubernetes/management` auth
mount as the `c5c3-operator` role with its pod's ServiceAccount token and the
shared client certificate of the ControlPlane's namespace, writes and deletes
the order roles, revokes their leases, and revokes its own token after each
use. Its `c5c3-operator` policy reaches nothing under `database/mariadb/config`,
so the MariaDB root password stays with the onboarding script. See
[OpenBao Bootstrap](../infrastructure/openbao-bootstrap.md).

It records no Events, for the reason the
[KeystoneUser reconciler](./keystoneuser-reconciler.md) gives.

## Controller Registration

```go
if err := (&controller.MariaDBDatabaseReconciler{
    Client:                  mgr.GetClient(),
    Scheme:                  mgr.GetScheme(),
    Resolver:                mcMgr,
    MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
}).SetupWithManager(mcMgr); err != nil { ... }
```

`Client` is the management cluster's client and `Resolver` the multicluster
manager. Two fields stay nil in production and are set by the tests:
`OpenBaoDial`, which defaults to `openbao.Login`, and `ServiceAccountToken`,
which defaults to reading
`/var/run/secrets/kubernetes.io/serviceaccount/token` on every call, because
the kubelet rotates the projected token in place.

The cluster-agnostic machinery (child naming and ownership, the admission
gates, the pass result, the teardown and the label mapping) is the shared
scaffold in `keystoneorder.go`. See
[The shared scaffold](./keystone-orders-reconciler.md#the-shared-scaffold).

Before the watches it registers one field indexer on the local manager's
indexer:

| Index | Key | Extractor |
| --- | --- | --- |
| `MariaDBDatabaseControlPlaneRefIndexKey` (`spec.controlPlaneRef`) | the resolved `<namespace>/<name>` of the reference | `mariaDBDatabaseControlPlaneRefExtractor` |

### Watches

| Resource | Watch | Clusters | Effect |
| --- | --- | --- | --- |
| `MariaDBDatabase` | `For()` | management, and every engaged target cluster that serves the kind | Filtered by `watch.CRUpdatePredicate()`. The request carries the event's cluster |
| `Secret` | `Owns()` | the same | The delivered Secret beside the order. An edit or a deletion brings the order back with the event's cluster |
| `Secret`, ESO `ExternalSecret`, ESO `VaultDynamicSecret` | `Watches()` | management | Mapped back to the order by the three ownership labels (`orderChildRequests`). The Secret ESO writes carries the labels through the ExternalSecret's target template, so each refresh brings the order back |
| mariadb-operator `Database` | `Watches()` | management, and every engaged target cluster that serves the kind | Mapped back by the same labels (`databaseChildRequests`), but only from the namespace a ControlPlane places Keystone in, on the cluster it places it on. The Database lives on Keystone's cluster while its cluster label names the order's. A Database CR an owner writes in an assigned namespace wakes no order, whatever its labels |
| `ControlPlane` | `Watches()` | management | Index-backed fan-out (`controlPlaneToMariaDBDatabasesMapper`) to the orders on the management cluster that reference it. Filtered by `orderControlPlanePredicateFor(DBCredentialsReady)`: an update passes on a spec change, a deletion, or a flip of `DBCredentialsReady` |

No ControlPlane watch reaches an order on a target cluster. Such an order is
reconciled again every 10 minutes (`orderRefreshAfter`), which is how a
`publishedDatabaseEndpoint` edit reaches it.

### RBAC

On the management cluster the ClusterRole gains the order verbs and the
mariadb-operator's `databases`:

```go
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases,verbs=get;list;watch;create;update;patch;delete
```

The ServiceAccounts, Certificates, VaultDynamicSecrets, ExternalSecrets and
Secrets it writes in the ControlPlane's namespace, and the ControlPlane and
MariaDB reads, are granted by the ControlPlane's marker block.

On a target cluster the `target-cluster-access` chart grants the same order
verbs and Secret write in each assigned namespace, and read on the kind in each
placed namespace. A placed Keystone's namespace already grants `databases`. See
[Assigned namespaces](../target-clusters.md#assigned-namespaces).

## Reconciliation Flow

```
Reconcile
  ├─ resolve the order's cluster ──→ unresolvable: log, no requeue
  ├─ deleting? ───────────────────→ reconcileDelete  (see Deletion and Teardown)
  └─ reconcileNormal
       ├─ orderAdmission
       │    ├─ cluster name ≤ 63   → ClusterNameTooLong, no requeue
       │    ├─ ControlPlane read   → ControlPlaneNotFound (10s); on a target
       │    │                        cluster NamespaceNotAssigned (1m)
       │    └─ namespace assignment → NamespaceNotAssigned (1m), frozen
       ├─ EnsureFinalizer          c5c3.io/mariadbdatabase-teardown
       ├─ mariaDBDatabaseDynamicGate
       │    ├─ not Dynamic         → DynamicCredentialsUnavailable (10m)
       │    └─ DBCredentialsReady  → WaitingForDBCredentials (10s)
       ├─ provisionDatabase  (instrumented: MariaDBDatabaseProvision)
       └─ deliverCredentials (instrumented: MariaDBDatabaseDelivery)
```

The assignment gate is the freeze: without an entry for the order's namespace
on its cluster, both conditions read `NamespaceNotAssigned` and the pass
returns before anything is read or written in the ControlPlane's namespace, in
Keystone's namespace or in OpenBao. The finalizer is installed only past this
gate.

The Dynamic gate decides with `effectiveKeystoneDatabase` and
`dbCredentialsDynamicEnabled`, the functions the ControlPlane's own DB
credential uses. Both refusals are written on both conditions and skip the
two steps.

When the provision step has not converged, `DeliveryReady` reads
`WaitingForCredentials` and the delivery is skipped. A delivered order on the
management cluster then waits for an event; every other order comes back after
10 minutes.

## Database provisioning

`provisionDatabase` writes `DatabaseReady` in seven steps:

1. Keystone's cluster is resolved (`TargetClusterUnavailable`), and the shared
   MariaDB `effectiveKeystoneDatabase(cp)` in Keystone's namespace has to be
   Ready (`ClusterNotReady`).
2. The schema name `mariaDBDatabaseName` is checked: a name that is not a
   MySQL identifier of 1 to 64 characters from `[A-Za-z0-9_]` is refused with
   `DatabaseNameInvalid`, a MariaDB system schema with `DatabaseNameReserved`,
   and a Database CR on the same MariaDB whose `spec.name` (or
   `metadata.name`) carries it and that the order did not create with
   `DatabaseCollision`. The collision message names the schema only, because
   the other Database CR may be another owner's order; the log line
   `the database an order names is taken on the shared MariaDB` names it under
   `takenBy`. The list reads the cache without copying it. No refusal requeues.
3. The ServiceAccount `order-db-creds` and the cert-manager Certificate
   `order-db-openbao-client` are applied in the ControlPlane's namespace,
   without labels and without an owner. Until cert-manager has written
   `tls.crt`, `tls.key` and `ca.crt` into the Secret of that name, the step
   reports `WaitingForClientCertificate`.
4. The operator logs in on OpenBao with that keypair and its ServiceAccount
   token, at the server and mount of the ControlPlane's store, and reads the
   role `order.<ControlPlane namespace>.<prefix without the trailing dash>` on
   `database/mariadb`. When the role is absent or differs, it writes it:
   `db_name` `keystone-<keystone namespace>`, one `CREATE USER` and one
   `GRANT ALL PRIVILEGES` on the schema with every `_` escaped as `\_`,
   `DROP USER IF EXISTS` on revocation, `default_ttl` `48h` and `max_ttl`
   `72h`. It revokes its own token before the step goes on. A failure writes
   `OpenBaoError`, prefixed with the three bootstrap scripts for a 403, and
   returns an error, so the pass backs off and counts in
   `c5c3_operator_reconcile_errors_total`. `status.roleName` is set.
5. The Database CR `<prefix>database` is applied in Keystone's namespace
   through that cluster's client, with `mariaDbRef.waitForIt: true` and the
   `cleanupPolicy` `spec.deletionPolicy` selects (`Skip` for `Retain`,
   `Delete` for `Delete`). It is applied on every pass, so a changed policy
   reaches the live CR. A live Database of that name the order did not create
   is refused with `DatabaseCollision`. Until the mariadb-operator reports it
   Ready, the step reports `WaitingForDatabase`.
6. The `VaultDynamicSecret` and the ExternalSecret `<prefix>credentials` are
   applied in the ControlPlane's namespace. The generator reads
   `database/mariadb/creds/<role>` as the `order-db` role with the shared
   ServiceAccount and keypair; the ExternalSecret refreshes every 24 hours and
   sets the order's labels on the Secret ESO writes. Until ESO reports the
   ExternalSecret Ready and that Secret carries a `password` and a username
   with the engine's `v-` prefix, the step reports `WaitingForCredentials`.
7. `status.databaseName` is set and `DatabaseReady` reads
   `DatabaseProvisioned`.

The engine connection the role names is the onboarding script's. Its
`allowed_roles` admits `order.<ControlPlane namespace>.*`, so until an existing
deployment re-runs `setup-database-tenant.sh` the role write in step 4 fails.

## Credential delivery

`deliverCredentials` writes `DeliveryReady` in six steps:

1. The address is resolved for the order's cluster (`mariaDBDatabaseEndpoint`):
   the in-cluster MariaDB Service on Keystone's cluster, the ControlPlane's
   `spec.infrastructure.publishedDatabaseEndpoint` elsewhere. Without one the
   step reports `DatabaseNotPublished` and writes nothing. A value that is not
   `host:port` with a port from 1 to 65535 reports `DeliveryError`.
2. When the shared database's `tls` block is enabled, `ca.crt` is read from its
   CA bundle Secret in Keystone's namespace (`WaitingForCABundle`).
3. The Secret ESO wrote, `<prefix>credentials` in the ControlPlane's
   namespace, has to carry a `username` and a `password`
   (`WaitingForCredentials`). The ExternalSecret's `status.refreshTime` is read
   beside it.
4. A live Secret `<name>-credentials` beside the order that the order does not
   control is refused (`DeliveryRefused`). On a target cluster that check reads
   the cluster's API server directly.
5. The `Opaque` Secret is applied beside the order with `host`, `port`,
   `database`, `username`, `password` and, from step 2, `ca.crt`, the name and
   namespace labels, and a controller owner reference to the order.
6. Once the apply response carries the current `username` and `password`,
   `status.host`, `status.port`, `status.credentialsRefreshedAt`,
   `status.secretName` and `status.secretKeys` are set and `DeliveryReady`
   reads `Delivered`. A rewrite that changes the username logs
   `delivered a refreshed database credential` with the order, the username
   and the refresh time, so the 24-hour cadence can be followed in the log.

The apply in step 5 is the repair, as for the KeystoneUser's delivered Secret:
an edited key is rewritten, a deleted Secret is applied again, a key another
field manager adds stays, and a `ca.crt` the operator owned is dropped once TLS
is off. A refresh is the same path: ESO rewrites `<prefix>credentials`, its
labels map the event back to the order, and the delivered Secret follows.

A converged pass costs a GET of the ControlPlane, the MariaDB, the Database
list, the Database, the client-certificate Secret, the ExternalSecret and two
Secrets, one OpenBao login, one role read and one `revoke-self`, plus the
applies the field manager finds unchanged. A converged order on the management
cluster waits for an event, so that is one login per change; an order on a
target cluster logs in once every 10 minutes.

## Child Naming and Placement

| Object | Name | Namespace | Cluster | Ownership |
| --- | --- | --- | --- | --- |
| Database CR | `<prefix>database` | Keystone's | Keystone's | labels |
| VaultDynamicSecret | `<prefix>credentials` | ControlPlane's | management | labels |
| ExternalSecret | `<prefix>credentials` | ControlPlane's | management | labels |
| Secret ESO writes | `<prefix>credentials` | ControlPlane's | management | ESO's owner reference, labels from the target template |
| OpenBao role | `order.<ControlPlane namespace>.<prefix without the trailing dash>` | | OpenBao | named by the order |
| ServiceAccount | `order-db-creds` | ControlPlane's | management | none, shared |
| Certificate and its Secret | `order-db-openbao-client` | ControlPlane's | management | none, shared |
| Delivered Secret | `<name>-credentials` | the order's | the order's | controller owner reference |

`<prefix>` is
`<name>-<first 8 hex of sha256(<cluster>/<namespace>/<name>)>-database-`. The
labels are `c5c3.io/mariadbdatabase-name`, `c5c3.io/mariadbdatabase-namespace`
and `c5c3.io/mariadbdatabase-cluster`, the last empty for the management
cluster.

The Database CR is the one child that lives beside the MariaDB, because the
mariadb-operator resolves `mariaDbRef` in the Database's own namespace. It is written through Keystone's cluster client, the
way the built-in schemas are.

## Deletion and Teardown

`reconcileDelete` runs the `c5c3.io/mariadbdatabase-teardown` finalizer through
`orderTeardown`, without consulting the assignment, and installs no hold:
nothing references a MariaDBDatabase. `sweepMariaDBDatabaseChildren` issues
the deletes in this order, `childNS` being the ControlPlane's namespace:

1. OpenBao: when the client-certificate Secret `order-db-openbao-client`
   exists in `childNS` with its three keys, the operator logs in, revokes every
   lease under `database/mariadb/creds/<role>` (which drops the SQL users),
   deletes the role and revokes its token. Without the Secret it logs
   `the OpenBao client certificate is gone; leaving database role "<role>" to its TTLs`
   and skips the step. With the ControlPlane present, an OpenBao error ends
   the pass with `removing OpenBao database role "<role>"` before anything is
   deleted. With the ControlPlane gone, the steps below run anyway and the
   errors are logged together.
2. The ExternalSecret and the VaultDynamicSecret in `childNS`. ESO reaps the
   Secret it wrote through its owner reference.
3. The Database CR in Keystone's namespace through Keystone's cluster client.
   Its `cleanupPolicy` is first patched to the one `spec.deletionPolicy`
   selects, because the provision writes it only on a pass that reaches step
   5, and a policy edit followed at once by the deletion reaches the
   reconciler as the deletion alone. With the ControlPlane gone, Keystone's
   namespace reads as `childNS` and the cluster as the management cluster.
4. The delivered Secret through the order's cluster client.

The sum of the issued deletes is the count `orderTeardown` holds the finalizer
on. With the ControlPlane present the finalizer is held while any child is
still listed, an error is retried, and the pass requeues after 10 seconds, so
the mariadb-operator runs the drop or the skip behind the Database CR's
finalizer first. With the ControlPlane gone the deletes are issued and the
finalizer is released at once, whatever their outcome.

## Metrics Instrumentation

Both steps run through the package-scope `instrumenter`, so they observe the
`c5c3_operator_reconcile_duration_seconds{sub_reconciler=…}` histogram and the
`c5c3_operator_reconcile_errors_total{sub_reconciler=…,condition_type=…}`
counter.

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `MariaDBDatabaseProvision` | `DatabaseReady` |
| `MariaDBDatabaseDelivery` | `DeliveryReady` |

The drift guard `TestSubReconcilerConditionTypesCoversAllNames` accepts a value
from `mariaDBDatabaseSubConditionTypes`, and
`TestSubReconcilerConditionTypes_MariaDBDatabaseLegs` pins the two names.

## Testing

| Layer | Location |
| --- | --- |
| Gates, freeze, naming, mappers, teardown | `operators/c5c3/internal/controller/mariadbdatabase_controller_test.go` |
| Database provisioning | `operators/c5c3/internal/controller/mariadbdatabase_provision_test.go` |
| Credential delivery | `operators/c5c3/internal/controller/mariadbdatabase_delivery_test.go` |
| The OpenBao client | `internal/common/openbao/client_test.go` |
| The bootstrap roles, policies and `allowed_roles` | `tests/unit/deploy/order_db_openbao_bootstrap_test.sh` |
| CRD schema | `TestIntegration_MariaDBDatabase_SchemaValidation` in `integration_test.go` |
| An order on a target cluster | `TestIntegration_Multicluster_ControlPlanePlacement` in `multicluster_integration_test.go` |
| End to end | `tests/e2e/c5c3/keystone-user/` |
| Admission rejection corpus | `tests/e2e/c5c3/invalid-mariadbdatabase-cr/` |

The unit tests drive a fake client per cluster and a recording fake OpenBao
client. The multicluster envtest runs two API servers and the same fake: an
order naming a plane that never reaches `DBCredentialsReady` holds with its
finalizer and writes nothing, and an order on the target cluster is provisioned
at home and delivered beside it once the plane publishes an endpoint. The
end-to-end suite is where the order meets OpenBao, ESO and the mariadb-operator.
See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
