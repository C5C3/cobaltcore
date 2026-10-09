---
title: KeystoneCatalogEntry CRD API Reference
quadrant: operator
---

# KeystoneCatalogEntry CRD API Reference

Reference documentation for the KeystoneCatalogEntry Custom Resource
Definition. A KeystoneCatalogEntry orders one entry in the Keystone catalog
from a namespace that a [ControlPlane](./controlplane-crd.md) assigns to a
service owner: a service row of a type and a name, and at most one endpoint row
per interface, registered in the ControlPlane's region. The c5c3-operator
registers the rows through K-ORC and reports their ids in status.

A catalog row is visible to every cloud user, so the namespace's
[`spec.namespaceAssignments`](./controlplane-crd.md#namespaceassignmentspec)
entry must set `allowCatalogEntries` as well. The entry alone does not admit an
order of this kind.

For the control loop, the watches and the teardown, see
[Keystone Orders Reconciler Architecture](./keystone-orders-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md) guide walks the
flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneCatalogEntry` |
| List Kind | `KeystoneCatalogEntryList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in
`keystonecatalogentry_types.go` registers both Kinds with the shared
`SchemeBuilder`.

`kubectl get keystonecatalogentry` prints four columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Type` | `.spec.serviceType` |
| `Age` | `.metadata.creationTimestamp` |

## Example

A `dns` service named `designate`, with a public and an internal endpoint:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneCatalogEntry
metadata:
  name: workflow-dns
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
  serviceType: dns
  serviceName: designate
  endpoints:
  - interface: public
    url: https://dns.example.test/v2
  - interface: internal
    url: http://designate-api.tenant-a.svc:9001/v2
```

The ControlPlane admits it with the namespace's entry:

```yaml
spec:
  namespaceAssignments:
  - namespace: tenant-a
    allowCatalogEntries: true
```

## Spec

### KeystoneCatalogEntrySpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | `ControlPlaneRefSpec` | Yes | — | The ControlPlane whose catalog the entry is registered in. Both halves are immutable. |
| `serviceType` | `string` | Yes | — | The OpenStack service type, a DNS-1123 label (`MaxLength=63`). `identity` is rejected; every other type, the built-in ones included, is admitted. **Immutable**. |
| `serviceName` | `string` | No | `metadata.name` | The catalog service name (`MinLength=1`, `MaxLength=255`, no comma, mirroring K-ORC's `OpenStackName`). **Immutable**: a rule on the whole object compares the effective name. |
| `endpoints` | [`[]KeystoneServiceEndpointSpec`](./keystoneservice-crd.md) | No | — | At most one endpoint row per `interface` (`public`, `internal`, `admin`), each with an `http`/`https` `url` of at most 1024 bytes. The list may change after creation. Without it the entry registers the service row alone. |

The endpoint type is KeystoneService's own, so the two kinds admit the same
endpoint rows.

Type and name are frozen because the collision probe runs only while no managed
Service exists: an in-place edit would reshape a registered row without probing
again. Delete and re-create the order to register another row.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the consent, as for a [KeystoneUser](./keystoneuser-crd.md#consent), and its
`allowCatalogEntries` must be `true`:

```
ControlPlane openstack/cp assigns namespace "tenant-a" on the management cluster without allowCatalogEntries (spec.namespaceAssignments); the order is frozen: nothing is registered, repaired or swept, and what was created stays
```

The flag binds orders only. The catalog block of a
[KeystoneService](./keystoneservice-crd.md) stays admitted by
`spec.korc.serviceRegistrations.allowedNamespaces`.

Without an entry `CatalogReady` reads `NamespaceNotAssigned`, and without the
flag `CatalogNotAllowed`. Either freezes the order: nothing is registered,
repaired or swept, and the rows it already has stay. An order its entry never
admitted carries no finalizer. Deleting the order is what removes the rows.

`identity` is the one reserved type. A row of any other type and the same name
that the order did not create is refused with `ServiceCollision`, never taken
over. Keystone-to-Keystone federation registers a remote Keystone under
`OS-FEDERATION`, not as a second `identity` row.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `CatalogReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `serviceID` | `string` | The Keystone service id, once the row is registered. |
| `serviceName` | `string` | The effective service name, once registered. |
| `endpoints` | `[]KeystoneServiceEndpointStatus` | One row per declared interface, with the Keystone endpoint `id` once Available. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `CatalogReady` | True | `CatalogRegistered` | The service row and every declared endpoint are Available in the ControlPlane's region. |
| `CatalogReady` | False | `CatalogNotAllowed` | The entry for the order's namespace does not set `allowCatalogEntries`. The order is frozen. |
| `CatalogReady` | False | `ServiceCollision` | A catalog row of this type and name exists that the order did not create. |
| `CatalogReady` | False | `ProbingForCollision` | The probe that decides whether the row already exists has not resolved yet. |
| `CatalogReady` | False | `WaitingForCatalog` | The Service, the Region import or an Endpoint is not Available yet, or an Endpoint the spec no longer declares is being removed. |
| `CatalogReady` | False | `CatalogFailed` | K-ORC reported a terminal error, on the Service first, then the Region import, then an Endpoint. |
| `CatalogReady` | False | `TransportErrorRetryFailed` | Clearing a latched transport error failed. |
| `CatalogReady` | False | `CatalogError` | A Kubernetes-level failure projecting a catalog child. |
| `CatalogReady` | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| `CatalogReady` | False | `NamespaceNotAssigned` | No entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| `CatalogReady` | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than 63 characters. |
| `CatalogReady` | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True. |
| `Ready` | True | `AllReady` | `CatalogReady` is True. |
| `Ready` | False | `NotAllReady` | `CatalogReady` is not True. |

## Defaulting and Validation Summary

The kind has **no webhook**, because a target cluster runs none. Every rule is a
CRD rule:

- `serviceType`: required, DNS-1123 label, `MaxLength=63`, the rule
  `self != 'identity'` ("the identity catalog entry is ControlPlane-owned and
  cannot be registered through a KeystoneCatalogEntry"), and a transition rule
  ("serviceType is immutable; delete and re-create the KeystoneCatalogEntry to
  register a different service type").
- `serviceName`: `MinLength=1`, `MaxLength=255`, pattern `^[^,]+$`.
- `endpoints`: `listType=map` keyed by `interface` (a second row of one
  interface is a `Duplicate value`), the `interface` enum, and the `url`
  pattern `^https?://[^\s/]+` with `MaxLength=1024`.
- `controlPlaneRef`: the two transition rules a KeystoneUser carries, naming
  KeystoneCatalogEntry in their messages.
- On the object: `metadata.name` is at most 63 bytes, and the effective service
  name is frozen ("serviceName is immutable; delete and re-create the
  KeystoneCatalogEntry to rename its catalog entry").

The rejection corpus lives in
`tests/e2e/c5c3/invalid-keystonecatalogentry-cr/`, generated from its
`_generate.py` and guarded by `make verify-invalid-cr-fixtures`.

## Projected child names

The children live in the ControlPlane's namespace on the management cluster,
named from a per-order prefix:

```
<metadata.name>-<8 hex>-catalogentry-<discriminator>
```

The discriminators are `service` (the managed K-ORC Service), `service-probe`
(the collision probe, dropped once the probe reports the row absent), `region`
(the unmanaged import of the ControlPlane's region) and
`endpoint-<interface>` (one managed Endpoint per declared interface). Each
child carries the labels `c5c3.io/keystonecatalogentry-name`,
`c5c3.io/keystonecatalogentry-namespace` and
`c5c3.io/keystonecatalogentry-cluster` and no owner reference.

## Deletion Semantics

The `c5c3.io/keystonecatalogentry-teardown` finalizer is installed once the
order's namespace is assigned and the entry admits catalog entries. Deleting a
KeystoneCatalogEntry that carries it runs the teardown, in this order:

| Resource | Fate |
| --- | --- |
| Endpoint rows | **Deleted** with the managed Endpoints |
| Service row | **Deleted** with the managed Service; a leftover probe is deleted with it |
| Region import | **Deleted**; an unmanaged import, so the region stays |

While the ControlPlane exists the finalizer is held until none of them is
listed: K-ORC holds the Region import while an Endpoint names it. Once the
ControlPlane is gone the teardown fails open. A frozen order tears down the same
way.

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. It orders
`workflow-dns` before the flag is set and reads `CatalogNotAllowed`, sets it,
reads the public URL out of the catalog from a Job in the owner's namespace,
refuses a second entry of the same row with `ServiceCollision`, freezes the
order by clearing the flag, and deletes it down to its K-ORC children.
`tests/e2e/c5c3/invalid-keystonecatalogentry-cr/` is the admission rejection
corpus, run on the `e2e-operator (c5c3)` job.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
