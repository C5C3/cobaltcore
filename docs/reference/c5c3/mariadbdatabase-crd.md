---
title: MariaDBDatabase CRD API Reference
quadrant: operator
---

# MariaDBDatabase CRD API Reference

Reference documentation for the MariaDBDatabase Custom Resource Definition. A
MariaDBDatabase orders one MariaDB database, and one user with grants on it,
from a namespace that a [ControlPlane](./controlplane-crd.md) assigns to a
service owner through
[`spec.namespaceAssignments`](./controlplane-crd.md#namespaceassignmentspec).
The c5c3-operator creates the schema on the ControlPlane's shared managed
MariaDB, writes an OpenBao database-engine role that issues users holding
`ALL PRIVILEGES` on that schema and nothing else, and copies each issued
credential into the Secret `<metadata.name>-credentials` beside the order.

The order lives in the assigned namespace, on the cluster that namespace is
assigned on: the management cluster or a registered
[target cluster](../target-clusters.md). Nothing in that namespace can reach
OpenBao. The owner receives a Secret and nothing else.

For the control loop, the watches and the teardown order, see
[MariaDBDatabase Reconciler Architecture](./mariadbdatabase-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md#order-a-database)
guide walks the flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `MariaDBDatabase` |
| List Kind | `MariaDBDatabaseList` |
| Plural | `mariadbdatabases` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in `mariadbdatabase_types.go`
registers both Kinds with the shared `SchemeBuilder`.

`kubectl get mariadbdatabase` prints five columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Database` | `.status.databaseName` |
| `Refreshed` | `.status.credentialsRefreshedAt` |
| `Age` | `.metadata.creationTimestamp` |

## Example

An order in the assigned namespace `tenant-a` against the ControlPlane `cp` in
`openstack`. The schema name defaults to `workflow_db`, the order's name with
its dash as an underscore, and the deletion policy to `Retain`.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: MariaDBDatabase
metadata:
  name: workflow-db
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
```

The ControlPlane assigns the namespace with an entry of its own:

```yaml
spec:
  namespaceAssignments:
  - namespace: tenant-a
```

## Spec

### MariaDBDatabaseSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | [`ControlPlaneRefSpec`](./keystoneuser-crd.md#controlplanerefspec) | Yes | — | The ControlPlane whose shared MariaDB the database is created on. Both halves are **immutable** (CEL transition rules). |
| `databaseName` | `string` | No | `metadata.name` with every `-` as `_` | The SQL schema name (`MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9_]+$`, the bounds of a MySQL identifier). **Immutable**: a rule on the whole object compares the effective name, so spelling out the default is admitted and any other value is rejected. The MariaDB system schemas are [reserved](#consent). |
| `deletionPolicy` | `string` | No | `Retain` | `Retain` or `Delete`. Decides whether deleting the order drops the schema and its data. Mutable; the value in force when the order is deleted applies. |

The reconciler resolves the `databaseName` default itself. The kind has no
defaulting webhook, so the stored object keeps the field empty. The schema
default fills in `deletionPolicy: Retain` on admission; a CR stored without the
field reads as `Retain`.

`controlPlaneRef` carries the rules and the namespace default of the
[KeystoneUser's](./keystoneuser-crd.md#controlplanerefspec), with messages
naming `MariaDBDatabase`.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the only consent a MariaDBDatabase reads, as for the
[Keystone orders](./keystoneuser-crd.md#consent). The entry's `allowedRoles` and
`allowCatalogEntries` are not consulted.

Without an entry both conditions read `False/NamespaceNotAssigned`. The order
is frozen: nothing is provisioned, delivered, repaired or swept, and the
schema, the OpenBao role, the ESO objects and the delivered Secret it already
has stay. The user OpenBao issued keeps working until its lease ends.
Withdrawing an entry is therefore not a revocation; deleting the order is.

The database is always the ControlPlane's shared managed database, the one its
Keystone uses. The OpenBao database engine holds a connection for that
database only, which `setup-database-tenant.sh` writes when it onboards the
ControlPlane. An order is refused with `DynamicCredentialsUnavailable` on both
conditions when that database issues no dynamic credentials:

| ControlPlane | Why it refuses |
| --- | --- |
| External mode | the ControlPlane manages no database |
| a brownfield `spec.infrastructure.database` (`host`) | the operator holds no root credential for it |
| `spec.services.keystone.dedicatedBackingServices.database` | the engine has no connection for a dedicated database |
| `credentialsMode: Static` | the engine connection is not in use |

A ControlPlane whose `DBCredentialsReady` is not `True` has not onboarded its
engine connection yet; the order reads `WaitingForDBCredentials` and checks
again every 10 seconds.

A schema the order did not create is never taken over. The mariadb-operator
creates a schema with `CREATE DATABASE IF NOT EXISTS`, which would adopt an
existing one silently, and the order's role would grant the owner
`ALL PRIVILEGES` on it. So the order refuses with `DatabaseNameReserved` a
`databaseName` that is a MariaDB system schema (`mysql`, `information_schema`,
`performance_schema`, `sys`, compared without regard to case), and with
`DatabaseCollision` a name another Database CR on the same MariaDB already
carries. A Database CR of the child's name that the order did not create is
refused with `DatabaseCollision` as well.

### Orders on a target cluster

An order for a namespace assigned on a target cluster is created on that
cluster. The operator watches the kind there, writes the order's status and
finalizer there, and writes the delivered Secret beside it. The OpenBao role,
the generator and the ExternalSecret stay in the ControlPlane's namespace on
the management cluster, and the Database CR beside the MariaDB on Keystone's
cluster.

The address the Secret carries is resolved for the order's cluster:

| Order on | `host` and `port` |
| --- | --- |
| the cluster the MariaDB runs on, which is Keystone's | the in-cluster Service, `<clusterRef>.<keystone namespace>.svc` and `3306` |
| any other cluster | [`spec.infrastructure.publishedDatabaseEndpoint`](./controlplane-crd.md#infrastructurespec), split at its last colon |

The operator publishes nothing itself. The platform operator exposes the
database port by their own means, for example a LoadBalancer Service or a
TCPRoute, and records the `host:port` in the ControlPlane. Without it an order
off the database's cluster reports `DeliveryReady=False/DatabaseNotPublished`
naming the field and receives nothing. Under `tls.mode: verify-full` the
MariaDB server certificate must carry the published host as a subject
alternative name.

The target cluster serves the kind through the `target-cluster-access` chart.
See [Assigned namespaces](../target-clusters.md#assigned-namespaces). An order
on a cluster registered under a name longer than 63 characters reports
`ClusterNameTooLong` on both conditions.

## Delivered Secret contract

| Property | Value |
| --- | --- |
| Name | `<metadata.name>-credentials`, also reported in `status.secretName` |
| Namespace | the order's own |
| Cluster | the order's own |
| Type | `Opaque` |
| Data keys | `host`, `port`, `database`, `username`, `password` and, when the shared database runs TLS, `ca.crt`; also reported in `status.secretKeys` in that order |
| Ownership | a controller owner reference to the order; the garbage collector reaps the Secret with it |
| Labels | `c5c3.io/mariadbdatabase-name` and `c5c3.io/mariadbdatabase-namespace` |

`host` and `port` are the address of the [order's cluster](#orders-on-a-target-cluster),
`port` as a decimal string. `database` is the resolved schema name. `username`
and `password` are the credential OpenBao issued last; the username starts with
`v-`. `ca.crt` is the `ca.crt` key of the shared database's
`tls.caBundleSecretRef` Secret, and is present only while the database's `tls`
block is enabled. A consumer that verifies the server passes it as the CA
bundle.

### Rotation cadence

The credential rotates on the cadence of the ControlPlane's own services, and
the cadence is not configurable:

| Step | Interval | Set by |
| --- | --- | --- |
| ESO reads a new credential from OpenBao, which creates a new SQL user | every 24 hours | the ExternalSecret's `refreshInterval` |
| OpenBao drops each user when its lease ends | 48 hours after it was issued | the role's `default_ttl` |
| Upper bound on any lease | 72 hours | the role's `max_ttl` |

Each refresh rewrites `username` and `password` in the delivered Secret and
moves `status.credentialsRefreshedAt`. A superseded user keeps working for 24
hours after the Secret moved on. A consumer therefore re-reads the Secret at
least once a day and reconnects with the new values, for example by mounting
the Secret as a volume and reloading on change. A consumer that reads the keys
into environment variables at start has to be restarted within that day.

The operator applies the Secret with Server-Side Apply and owns its keys. An
edited key is rewritten and a deleted Secret is written again, both on the
event the edit raises. A key somebody else adds stays. A `ca.crt` the operator
wrote is dropped when the database's TLS is turned off. A
[frozen](#consent) order repairs nothing. A Secret of the same name that the
order does not own is never taken over: the order reports `DeliveryRefused`
and leaves it unchanged.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `DatabaseReady`, `DeliveryReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `databaseName` | `string` | The resolved schema name. |
| `host` | `string` | The database host as delivered. |
| `port` | `int32` | The database port as delivered. |
| `roleName` | `string` | The OpenBao database-engine role that issues the order's users. |
| `credentialsRefreshedAt` | `*metav1.Time` | When ESO last issued the credential the delivered Secret carries, the ExternalSecret's `status.refreshTime`. |
| `secretName` | `string` | The [delivered Secret](#delivered-secret-contract), once it carries the current credential. |
| `secretKeys` | `[]string` | The keys of the delivered Secret, once it carries the current credential. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `DatabaseReady` | True | `DatabaseProvisioned` | The schema exists, the OpenBao role issues its users, and ESO has issued a credential. |
| `DatabaseReady` | False | `ClusterNotReady` | The shared MariaDB is missing or not Ready. |
| `DatabaseReady` | False | `DatabaseNameInvalid` | The effective schema name is not a MySQL identifier of 1 to 64 characters from `[A-Za-z0-9_]`. The CRD rejects such a name; the reconciler checks it again before the name reaches SQL. |
| `DatabaseReady` | False | `DatabaseNameReserved` | `databaseName` is a MariaDB system schema. |
| `DatabaseReady` | False | `DatabaseCollision` | Another Database CR on the same MariaDB carries the schema, or a Database CR of the child's name exists that the order did not create. The message names the schema only; the operator's log names the other Database CR. |
| `DatabaseReady` | False | `WaitingForClientCertificate` | cert-manager has not issued the shared OpenBao client certificate `order-db-openbao-client` yet. |
| `DatabaseReady` | False | `OpenBaoError` | The operator's login on OpenBao or its role write failed, with OpenBao's error. A 403 names the three bootstrap scripts to re-run. |
| `DatabaseReady` | False | `WaitingForDatabase` | The mariadb-operator has not reported the Database CR Ready yet. |
| `DatabaseReady` | False | `WaitingForCredentials` | ESO has not issued a credential from the role yet, or the Secret it wrote carries no engine-issued username. |
| `DatabaseReady` | False | `TargetClusterUnavailable` | The cluster the MariaDB runs on, Keystone's, does not resolve. |
| `DeliveryReady` | True | `Delivered` | The Secret beside the order carries the current credential. |
| `DeliveryReady` | False | `WaitingForCredentials` | The database is not provisioned yet, or the delivered Secret does not carry the current credential yet. |
| `DeliveryReady` | False | `DatabaseNotPublished` | The order lives on a cluster that cannot reach the in-cluster MariaDB Service and the ControlPlane publishes no database endpoint. |
| `DeliveryReady` | False | `WaitingForCABundle` | The shared database runs TLS and its CA bundle Secret does not carry `ca.crt` yet. |
| `DeliveryReady` | False | `DeliveryRefused` | A Secret of the delivered name exists that the order does not own. |
| `DeliveryReady` | False | `DeliveryError` | A Kubernetes-level failure writing or reading a delivery object, or a `publishedDatabaseEndpoint` that is not `host:port` with a port from 1 to 65535, with the error text. |
| both | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| both | False | `NamespaceNotAssigned` | No `spec.namespaceAssignments` entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| both | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than the 63 characters a label value carries. |
| both | False | `DynamicCredentialsUnavailable` | The ControlPlane's shared database issues no dynamic credentials. See [Consent](#consent). |
| both | False | `WaitingForDBCredentials` | The ControlPlane's `DBCredentialsReady` is not True; its engine connection is not onboarded yet (`setup-database-tenant.sh`). |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

An order on the management cluster is reconciled again when its ControlPlane
changes spec, is deleted, or flips `DBCredentialsReady`, and on every event of
its children. An order on a target cluster, and a refusal that only an edit
elsewhere lifts (`DatabaseNotPublished`, `DeliveryRefused`,
`DynamicCredentialsUnavailable`), is reconciled again every 10 minutes.

## Defaulting and Validation Summary

The kind has **no webhook**, defaulting or validating, because a target
cluster runs none. Every rule is a CRD rule the API server enforces on any
cluster that serves the kind:

- `databaseName`: `MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9_]+$`.
- `deletionPolicy`: enum `Retain`, `Delete`; schema default `Retain`.
- `controlPlaneRef.name`: required, `MinLength=1`, and a transition rule
  ("controlPlaneRef.name is immutable").
- `controlPlaneRef.namespace`: RFC-1123 label, ≤ 63, and a transition rule
  ("controlPlaneRef.namespace is immutable").
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes"), because it is a label value on the order's children; the
  effective schema name is frozen ("databaseName is immutable"); and an order
  without `databaseName` needs a name that is a MySQL identifier once its
  dashes are underscores, `^[a-z0-9-]{1,64}$` ("metadata.name is not a MySQL
  identifier once its dashes are underscores; set spec.databaseName"). A name
  such as `app.v1` is admitted with an explicit `databaseName`.

Cross-object rules live in the reconciler: whether the ControlPlane exists and
assigns the namespace, whether its database issues dynamic credentials, and
whether the schema collides each need a read admission does not have. The
reconciler also checks the effective schema name against
`^[A-Za-z0-9_]{1,64}$` again (`DatabaseNameInvalid`): the name reaches SQL that
OpenBao runs as the MariaDB root user, and the CRD on a target cluster is
installed by that cluster's administrator.

The rejection corpus lives in `tests/e2e/c5c3/invalid-mariadbdatabase-cr/`,
generated from its `_generate.py` and guarded by
`make verify-invalid-cr-fixtures`.

## Projected child names

The children of an order are named from a per-order prefix:

```
<metadata.name>-<8 hex>-database-<discriminator>
```

The hash covers `<cluster>/<namespace>/<name>`, with the empty string for the
management cluster, so the same order name in another namespace or on another
cluster never shares a child name.

| Child | Name | Namespace | Cluster |
| --- | --- | --- | --- |
| mariadb-operator Database CR | `<prefix>database` | Keystone's | Keystone's |
| ESO `VaultDynamicSecret` | `<prefix>credentials` | ControlPlane's | management |
| ESO `ExternalSecret` and the Secret it writes | `<prefix>credentials` | ControlPlane's | management |
| OpenBao database-engine role | `order.<ControlPlane namespace>.<prefix without the trailing dash>` | | OpenBao |

None of them can carry an owner reference to the order, so each carries three
labels instead: `c5c3.io/mariadbdatabase-name`,
`c5c3.io/mariadbdatabase-namespace` and `c5c3.io/mariadbdatabase-cluster` (empty
for the management cluster). The ExternalSecret's target template sets them on
the Secret ESO writes, so each refresh brings the order back.

The role name uses dots as separators because a namespace cannot contain one:
the generators' `order-db-dynamic` policy reads
`database/mariadb/creds/order.<own namespace>.*` and reaches the roles of one
namespace only. The SQL usernames OpenBao derives from the role stay
`v-<display>-<role>-<random>-<time>`, truncated to 32 characters.

Every database order of a ControlPlane namespace shares two objects there: the
ServiceAccount `order-db-creds` the generators present to OpenBao, and the
cert-manager Certificate `order-db-openbao-client`, whose keypair the generators
and the operator's own OpenBao client present to the listener. They carry no
order labels, no order sweeps them, and they leave with the namespace.

## Deletion Semantics

The `c5c3.io/mariadbdatabase-teardown` finalizer is installed once the order's
namespace is assigned, before anything is created. An order that never got past
the assignment carries none and is deleted at once. Deleting a MariaDBDatabase
that carries it runs the teardown:

| Resource | `Retain` | `Delete` |
| --- | --- | --- |
| Every SQL user the role issued | **Dropped**: the operator revokes the role's leases | **Dropped** |
| OpenBao role | **Deleted** | **Deleted** |
| `VaultDynamicSecret` and `ExternalSecret` | **Deleted**; ESO reaps the Secret it wrote | **Deleted** |
| Database CR | **Deleted** with `cleanupPolicy: Skip` | **Deleted** with `cleanupPolicy: Delete` |
| Schema and its data | **Kept** | **Dropped** by the mariadb-operator |
| Delivered Secret | **Deleted**; the garbage collector would reap it too | **Deleted** |

The operator writes `spec.deletionPolicy` into the Database CR's
`cleanupPolicy` on every pass, and once more in the teardown before it deletes
the CR, so the policy in force when the order is deleted applies, also when no
pass got as far as the Database CR since it changed. Revoking the leases first means a deleted order reaches nothing
from the moment its teardown runs, not up to 72 hours later.

The assignment is not consulted, so a frozen order tears down the same way.
While the ControlPlane exists the teardown is patient: a failing OpenBao call
is retried with back-off, and the finalizer is held until no child is listed
any more, so the mariadb-operator can drop or release the schema first. A
persistent OpenBao outage therefore holds the order's deletion; restore OpenBao,
or remove the finalizer by hand and the role with
`bao delete database/mariadb/roles/<status.roleName>`.

Once the ControlPlane is gone the teardown fails open: it still removes the
role through the client certificate in the ControlPlane's namespace, issues the
deletes, and releases the finalizer whatever their outcome. A failing OpenBao
call does not keep the deletes from being issued. Without that
certificate the operator cannot log in; it logs
`the OpenBao client certificate is gone; leaving database role "<role>" to its TTLs`,
every lease ends at its own `max_ttl`, and the role stays until an operator
deletes it by hand. A Keystone the gone plane had placed on a target cluster
leaves its Database CR behind there.

ESO could issue one more lease between the revocation and the deletion of the
ExternalSecret in the same pass. With the ControlPlane present the next pass
revokes it again; with the plane gone it ends at its 72-hour `max_ttl`.

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. Its
database step orders `workflow-db` from an assigned namespace, checks the role,
the Database CR and the ESO objects in the plane's namespace and nothing in the
owner's, writes and reads a row from a Job with the delivered Secret, repairs
an edited and a deleted Secret, forces an ESO refresh, freezes the order by
withdrawing the assignment, and deletes it: the last credential stops
authenticating, the role is gone, and `workflow_db` is kept under `Retain` and
dropped after a re-order with `Delete`.
`tests/e2e/c5c3/invalid-mariadbdatabase-cr/` is the admission rejection corpus,
run on the `e2e-operator (c5c3)` job. The target-cluster path is proven in the
two-API-server envtest `TestIntegration_Multicluster_ControlPlanePlacement`.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
