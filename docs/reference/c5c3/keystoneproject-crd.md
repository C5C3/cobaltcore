---
title: KeystoneProject CRD API Reference
quadrant: operator
---

# KeystoneProject CRD API Reference

Reference documentation for the KeystoneProject Custom Resource Definition. A
KeystoneProject orders one Keystone project from a namespace that a
[ControlPlane](./controlplane-crd.md) assigns to a service owner through
[`spec.namespaceAssignments`](./controlplane-crd.md#namespaceassignmentspec).
The c5c3-operator creates the project through K-ORC in the ControlPlane's admin
domain, where every ordered [KeystoneUser](./keystoneuser-crd.md) lives as
well, and reports its id in status. No Secret is delivered: a
[KeystoneRoleAssignment](./keystoneroleassignment-crd.md) gives an ordered user
a role on the project, and the user's own Secret scopes a token to it.

The order lives in the assigned namespace, on the cluster that namespace is
assigned on: the management cluster or a registered
[target cluster](../target-clusters.md). Its K-ORC Project lives in the
ControlPlane's namespace on the management cluster.

For the control loop, the watches and the teardown, see
[Keystone Orders Reconciler Architecture](./keystone-orders-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md) guide walks the
flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneProject` |
| List Kind | `KeystoneProjectList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in `keystoneproject_types.go`
registers both Kinds with the shared `SchemeBuilder`.

`kubectl get keystoneproject` prints four columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Project` | `.status.projectName` |
| `Age` | `.metadata.creationTimestamp` |

## Example

A project ordered from the assigned namespace `tenant-a` against the
ControlPlane `cp` in `openstack`. The project name defaults to `metadata.name`.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneProject
metadata:
  name: workflow-project
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
```

## Spec

### KeystoneProjectSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | `ControlPlaneRefSpec` | Yes | — | The ControlPlane whose identity plane the project is created in. Both halves are immutable, as on a [KeystoneUser](./keystoneuser-crd.md#controlplanerefspec). |
| `projectName` | `string` | No | `metadata.name` | The Keystone project name (`MinLength=1`, `MaxLength=255`, no comma, mirroring K-ORC's `KeystoneName`). **Immutable**: a rule on the whole object compares the effective name. The admin project and the built-in service projects are [reserved](#conditions). |

The reconciler resolves the `projectName` default itself. The kind has no
defaulting webhook, so the stored object keeps the field empty.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the only consent, as for a [KeystoneUser](./keystoneuser-crd.md#consent). The
order requests no role, so the entry's `allowedRoles` is not consulted.

Without an entry `ProjectReady` reads `False/NamespaceNotAssigned`. The order is
frozen: nothing is provisioned, repaired or swept, and the Keystone project it
already has stays. Deleting the order still tears it down.

While a KeystoneRoleAssignment in the order's namespace names the order as its
`projectRef`, deleting the order holds on `ReferencedByRoleAssignments`, and
while a [KeystoneApplicationCredential](./keystoneapplicationcredential-crd.md)
does, on `ReferencedByApplicationCredentials`. See
[Deletion Semantics](#deletion-semantics).

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `ProjectReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `projectID` | `string` | The Keystone project id, once provisioned. |
| `projectName` | `string` | The effective project name, once provisioned. |
| `domainName` | `string` | The ControlPlane's admin domain, once provisioned. A consumer sets it as `OS_PROJECT_DOMAIN_NAME`. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `ProjectReady` | True | `ProjectProvisioned` | The Keystone project exists in the admin domain. |
| `ProjectReady` | False | `ProjectCollision` | A project of this name exists in the admin domain that the order did not create, or the name is one the ControlPlane creates itself: its admin project, or a built-in service project (`service-glance`, `service-placement`, `service-barbican`, `service-neutron`, `service-cinder`, `service-nova`), whether or not that service is enabled. Names are compared as Keystone compares them, ignoring case and accents. The order never takes a project over. |
| `ProjectReady` | False | `ProbingForCollision` | The probe that decides whether the project already exists has not resolved yet. |
| `ProjectReady` | False | `WaitingForProject` | The managed Project is not Available yet. |
| `ProjectReady` | False | `ProjectFailed` | K-ORC reported a terminal error on the Project. |
| `ProjectReady` | False | `TransportErrorRetryFailed` | Clearing a latched transport error from the Project failed. |
| `ProjectReady` | False | `ProjectError` | A Kubernetes-level failure projecting the project. |
| `ProjectReady` | False | `ReferencedByRoleAssignments` | The order is being deleted while KeystoneRoleAssignments in its namespace still name it. The message lists them. |
| `ProjectReady` | False | `ReferencedByApplicationCredentials` | The order is being deleted while KeystoneApplicationCredentials in its namespace still name it. The message lists them. |
| `ProjectReady` | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| `ProjectReady` | False | `NamespaceNotAssigned` | No `spec.namespaceAssignments` entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| `ProjectReady` | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than the 63 characters a label value carries. |
| `ProjectReady` | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True. |
| `Ready` | True | `AllReady` | `ProjectReady` is True. |
| `Ready` | False | `NotAllReady` | `ProjectReady` is not True. |

Against an External-mode ControlPlane a wait on the Project is replaced by the
classified K-ORC failure, as for a KeystoneUser. An order on a target cluster,
and a reserved name, is reconciled again every 10 minutes.

## Defaulting and Validation Summary

The kind has **no webhook**, because a target cluster runs none. Every rule is a
CRD rule:

- `projectName`: `MinLength=1`, `MaxLength=255`, pattern `^[^,]+$`.
- `controlPlaneRef.name`: required, `MinLength=1`, and a transition rule
  ("controlPlaneRef.name is immutable; delete and re-create the KeystoneProject
  to move it to another ControlPlane").
- `controlPlaneRef.namespace`: RFC-1123 label, ≤ 63, and a transition rule
  ("controlPlaneRef.namespace is immutable; ..."), spelled `__namespace__` in
  CEL.
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes"), and the effective project name is frozen ("projectName is
  immutable; delete and re-create the KeystoneProject to rename its project").

The rejection corpus lives in `tests/e2e/c5c3/invalid-keystoneproject-cr/`,
generated from its `_generate.py` and guarded by
`make verify-invalid-cr-fixtures`.

## Projected child names

The children live in the ControlPlane's namespace on the management cluster,
named from a per-order prefix:

```
<metadata.name>-<8 hex>-project-<discriminator>
```

The hash covers `<cluster>/<namespace>/<name>`, as for a KeystoneUser. The
discriminators are `project` (the managed K-ORC Project, which a
KeystoneRoleAssignment names as its `projectRef`) and `project-probe` (the
collision probe). Each child carries the labels `c5c3.io/keystoneproject-name`,
`c5c3.io/keystoneproject-namespace` and `c5c3.io/keystoneproject-cluster`
(empty for the management cluster) and no owner reference.

## Deletion Semantics

The `c5c3.io/keystoneproject-teardown` finalizer is installed once the order's
namespace is assigned. Deleting a KeystoneProject that carries it runs the
teardown:

| Resource | Fate |
| --- | --- |
| Keystone project | **Deleted** with the managed K-ORC Project |
| Collision probe | **Deleted**; an unmanaged import, so the project it found stays |

The teardown first lists the KeystoneRoleAssignments in the order's namespace.
While one names the order as its `projectRef`, the order reads
`ProjectReady=False/ReferencedByRoleAssignments`, deletes nothing, and checks
again every minute and whenever an assignment changes. K-ORC guards a Project
with a finalizer while a RoleAssignment references it, and removing the project
would take the assignment with it. Delete the assignments first.

The teardown holds the same way while a KeystoneApplicationCredential names the
order as its `projectRef`, reading
`ProjectReady=False/ReferencedByApplicationCredentials`: the credentials are
scoped to the project. Delete the credential orders first.

Past the hold the teardown is the KeystoneUser one: patient while the
ControlPlane exists, failing open once it is gone, and not consulting the
assignment, so a frozen order tears down the same way.

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. It orders
`workflow-project` beside the user, reads its id, assigns a role on it, scopes
a token to it from a Job in the owner's namespace, freezes it with the
assignment withdrawn, and deletes it down to its K-ORC Project.
`tests/e2e/c5c3/invalid-keystoneproject-cr/` is the admission rejection corpus,
run on the `e2e-operator (c5c3)` job. The target-cluster path is proven in the
two-API-server envtest `TestIntegration_Multicluster_ControlPlanePlacement`.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
