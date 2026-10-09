---
title: KeystoneRoleAssignment CRD API Reference
quadrant: operator
---

# KeystoneRoleAssignment CRD API Reference

Reference documentation for the KeystoneRoleAssignment Custom Resource
Definition. A KeystoneRoleAssignment orders one Keystone role for the user of a
[KeystoneUser](./keystoneuser-crd.md) on the project of a
[KeystoneProject](./keystoneproject-crd.md), both ordered from the same
namespace. The c5c3-operator assigns the role through K-ORC and reports the
role, user and project ids in status. No Secret is delivered: the user's own
Secret scopes a token to the project through `OS_PROJECT_NAME` and
`OS_PROJECT_DOMAIN_NAME`.

The role must be on the `allowedRoles` of the namespace's
[`spec.namespaceAssignments`](./controlplane-crd.md#namespaceassignmentspec)
entry. No other order kind reads that list.

For the control loop, the watches and the teardown, see
[Keystone Orders Reconciler Architecture](./keystone-orders-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md) guide walks the
flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneRoleAssignment` |
| List Kind | `KeystoneRoleAssignmentList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in
`keystoneroleassignment_types.go` registers both Kinds with the shared
`SchemeBuilder`.

`kubectl get keystoneroleassignment` prints six columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `User` | `.spec.userRef.name` |
| `Project` | `.spec.projectRef.name` |
| `Role` | `.spec.role` |
| `Age` | `.metadata.creationTimestamp` |

## Example

The role `member` for the ordered user `workflow` on the ordered project
`workflow-project`, all in `tenant-a`:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneRoleAssignment
metadata:
  name: workflow-member
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
  userRef:
    name: workflow
  projectRef:
    name: workflow-project
  role: member
```

The ControlPlane admits the role with the namespace's entry:

```yaml
spec:
  namespaceAssignments:
  - namespace: tenant-a
    allowedRoles: [member]
```

## Spec

### KeystoneRoleAssignmentSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | `ControlPlaneRefSpec` | Yes | — | The ControlPlane whose identity plane the role is assigned in. Both referenced orders must name the same ControlPlane. Both halves are immutable. |
| `userRef.name` | `string` | Yes | — | The KeystoneUser in the order's namespace on the order's cluster (`MinLength=1`, `MaxLength=63`). **Immutable**. |
| `projectRef.name` | `string` | Yes | — | The KeystoneProject in the order's namespace on the order's cluster (`MinLength=1`, `MaxLength=63`). **Immutable**. |
| `role` | `string` | Yes | — | The Keystone role name (`MinLength=1`, `MaxLength=255`, no comma, mirroring K-ORC's `KeystoneName`), matched against `allowedRoles` by its case-sensitive name. **Immutable**. |

The references name orders, not Keystone names. A KeystoneService account, a
project another namespace ordered, or a Keystone name no order created cannot
be referenced. The scope is always a project; there is no domain or system
scope and no group actor.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the consent, as for a [KeystoneUser](./keystoneuser-crd.md#consent), and its
`allowedRoles` is the role allowlist. A role outside the list reads
`AssignmentReady=False/RoleNotAllowed`, with a message that names the list and
the refused role:

```
ControlPlane openstack/cp admits roles [] from namespace "tenant-a" on the management cluster (spec.namespaceAssignments); ["member"] is not on that list
```

An absent `allowedRoles` admits no role. Keystone's implied roles still apply
to what an allowed role grants: `member` implies `reader`.

Removing the role from the list, or the entry altogether, freezes the order the
way a withdrawn entry freezes a KeystoneUser (#1327 D2): nothing is projected,
repaired or swept, and an assignment that already exists in Keystone stays.
Deleting the order is what revokes the role.

### Duplicate assignments

Two orders in one namespace with the same `userRef`, `projectRef` and `role`
would project two K-ORC RoleAssignments onto one Keystone assignment, and
deleting either would unassign the role the other still declares. The younger
order, by creation time and then by name, reads
`AssignmentReady=False/DuplicateRoleAssignment` naming the older one and
projects nothing. It is checked again every 10 minutes, so it can take that
long to pick the role up after the older order is deleted.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `AssignmentReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `roleID` | `string` | The Keystone role id, once assigned. |
| `userID` | `string` | The Keystone id of the referenced user, once assigned. |
| `projectID` | `string` | The Keystone id of the referenced project, once assigned. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `AssignmentReady` | True | `RoleAssigned` | K-ORC reports the role assigned to the user on the project. |
| `AssignmentReady` | False | `RoleNotAllowed` | The role is not on the entry's `allowedRoles`. The order is frozen. |
| `AssignmentReady` | False | `UserNotFound` | No KeystoneUser of `userRef.name` is in the order's namespace on its cluster. |
| `AssignmentReady` | False | `ProjectNotFound` | No KeystoneProject of `projectRef.name` is in the order's namespace on its cluster. |
| `AssignmentReady` | False | `ControlPlaneMismatch` | A referenced order names another ControlPlane than this order. |
| `AssignmentReady` | False | `WaitingForUser` | The referenced user's `UserReady` is not True. The message carries its reason. |
| `AssignmentReady` | False | `WaitingForProject` | The referenced project's `ProjectReady` is not True. The message carries its reason. |
| `AssignmentReady` | False | `DuplicateRoleAssignment` | An older order in the namespace assigns the same role to the same user on the same project. |
| `AssignmentReady` | False | `WaitingForServiceAccounts` | The Role import or the RoleAssignment is not Available yet. Past two minutes the message adds that the role may not exist in Keystone. |
| `AssignmentReady` | False | `RoleAssignmentFailed` | K-ORC reported a terminal error on the Role import or the RoleAssignment. |
| `AssignmentReady` | False | `RoleAssignmentError` | A Kubernetes-level failure projecting the assignment. |
| `AssignmentReady` | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| `AssignmentReady` | False | `NamespaceNotAssigned` | No entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| `AssignmentReady` | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than 63 characters. |
| `AssignmentReady` | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True. |
| `Ready` | True | `AllReady` | `AssignmentReady` is True. |
| `Ready` | False | `NotAllReady` | `AssignmentReady` is not True. |

A change of a referenced order's spec, deletion, `Ready` or gating condition
brings the assignment back at once. A refusal the allowlist causes is lifted by
the ControlPlane watch for an order on the management cluster, and on the
10-minute refresh for one on a target cluster.

## Defaulting and Validation Summary

The kind has **no webhook**, because a target cluster runs none. Every rule is a
CRD rule:

- `userRef`, `projectRef`, `role`: required.
- `userRef.name`, `projectRef.name`: `MinLength=1`, `MaxLength=63`, and a
  transition rule each ("userRef is immutable; delete and re-create the
  KeystoneRoleAssignment to bind another user", "projectRef is immutable;
  delete and re-create the KeystoneRoleAssignment to bind another project").
- `role`: `MinLength=1`, `MaxLength=255`, pattern `^[^,]+$`, and a transition
  rule ("role is immutable; delete and re-create the KeystoneRoleAssignment to
  assign another role").
- `controlPlaneRef`: the two transition rules a KeystoneUser carries, naming
  KeystoneRoleAssignment in their messages.
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes").

The rejection corpus lives in
`tests/e2e/c5c3/invalid-keystoneroleassignment-cr/`, generated from its
`_generate.py` and guarded by `make verify-invalid-cr-fixtures`.

## Projected child names

The children live in the ControlPlane's namespace on the management cluster,
named from a per-order prefix:

```
<metadata.name>-<8 hex>-roleassignment-<discriminator>
```

The discriminators are `role` (the unmanaged Role import, filtered by the role
name) and `assignment` (the managed RoleAssignment). The RoleAssignment binds
the user's managed User, `<user prefix>user`, to the project's managed Project,
`<project prefix>project`, and authenticates through the operator's admin
password cloud, so its deletion still reaches Keystone after an application
credential is revoked. Each child carries the labels
`c5c3.io/keystoneroleassignment-name`,
`c5c3.io/keystoneroleassignment-namespace` and
`c5c3.io/keystoneroleassignment-cluster` and no owner reference.

## Deletion Semantics

The `c5c3.io/keystoneroleassignment-teardown` finalizer is installed once the
order's namespace is assigned. Deleting a KeystoneRoleAssignment that carries
it runs the teardown:

| Resource | Fate |
| --- | --- |
| Keystone role assignment | **Deleted**: K-ORC unassigns the role before it releases the RoleAssignment |
| Role import | **Deleted**; an unmanaged import, so the role stays in Keystone |
| Referenced user and project | Unchanged |

While the ControlPlane exists the finalizer is held until neither child is
listed. Once it is gone the teardown fails open. A frozen order tears down the
same way.

While the order exists, deleting the referenced KeystoneUser or KeystoneProject
holds on `ReferencedByRoleAssignments`. Delete the assignments first; the held
order then proceeds on its own.

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. It orders
`workflow-member` before the role is allowed and reads `RoleNotAllowed`, admits
the role, scopes a token to the project from a Job in the owner's namespace,
refuses a duplicate order of the same tuple, freezes the order by taking the
role off the list, and deletes it after holding the user's deletion.
`tests/e2e/c5c3/invalid-keystoneroleassignment-cr/` is the admission rejection
corpus, run on the `e2e-operator (c5c3)` job.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
