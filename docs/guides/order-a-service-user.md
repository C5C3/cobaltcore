---
title: Order a Service User
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Order a Service User

A service owner whose namespace a ControlPlane assigns can order a Keystone user
there with a `KeystoneUser` CR. The c5c3-operator creates the user, backs its
password up to OpenBao, and writes the credentials into a Secret beside the
order. Nothing in the owner's namespace can reach OpenBao: the owner receives a
Secret and nothing else.

Beside the user the owner can order a project, a role for the user on it, a
catalog entry and an application credential, each as a CR of its own. The
user's Secret scopes a token to the project once the role is assigned. Of the
further orders only the application credential delivers a Secret, which the
operator rotates on a schedule.

The example orders a user for a fictional `workflow` service from a namespace
of the same name.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

---

## Background: what lands where

| Description | Namespace | Why |
| --- | --- | --- |
| The namespace assignment | `openstack` | It is consent the ControlPlane gives, so it lives on the ControlPlane CR |
| The K-ORC User, its password Secrets, the source Secret and the PushSecret | `openstack` | K-ORC reads the admin credential there, and the PushSecret pushes the password to OpenBao through that namespace's own secret store |
| The order and the delivered Secret `workflow-credentials` | `workflow` | The credentials are delivered where the service that reads them runs |
| The K-ORC Project, Role import, RoleAssignment, Service, Region import and Endpoints of the further orders | `openstack` | K-ORC reads the admin credential there |
| The `KeystoneProject`, `KeystoneRoleAssignment` and `KeystoneCatalogEntry` orders | `workflow` | They live beside the user they belong to |
| The K-ORC ApplicationCredentials, their secrets, the user's `mint-cloud` document, the source Secret and the PushSecret of the application credential | `openstack` | K-ORC creates and deletes the credentials there, authenticated as the user |
| The `KeystoneApplicationCredential` order and its Secret `workflow-appcred-credentials` | `workflow` | The credential is delivered where the service that reads it runs |

The objects in `openstack` cannot carry an owner reference to an order in
another namespace, so they carry the labels `c5c3.io/keystoneuser-name`,
`c5c3.io/keystoneuser-namespace` and `c5c3.io/keystoneuser-cluster` instead.

## Steps

### 1. Create the service's namespace

```bash
kubectl create namespace workflow
```

### 2. Assign the namespace on the ControlPlane

The assignment is the only consent an order reads. The list is atomic, and a
merge patch replaces every entry in it, so change it with a JSON patch. On a
ControlPlane that assigns no namespace yet, create the list:

```bash
kubectl patch controlplane controlplane -n openstack --type json \
  -p '[{"op":"add","path":"/spec/namespaceAssignments","value":[{"namespace":"workflow"}]}]'
```

On a ControlPlane that already assigns namespaces, append an entry instead:

```bash
kubectl patch controlplane controlplane -n openstack --type json \
  -p '[{"op":"add","path":"/spec/namespaceAssignments/-","value":{"namespace":"workflow"}}]'
```

The entry lists no roles: the ordered user has no project and no role, so the
entry's `allowedRoles` plays no part.

### 3. Order the user

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: KeystoneUser
metadata:
  name: workflow
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
EOF
```

The user name defaults to `metadata.name` and the password generation to 1.
`controlPlaneRef.namespace` is set because the order lives in another namespace
than the ControlPlane.

### 4. Wait for the order to converge

K-ORC creates the user in Keystone, and ESO pushes the password to OpenBao
before anything is delivered:

```bash
kubectl wait --for=condition=Ready keystoneuser/workflow -n workflow --timeout=15m
kubectl get keystoneuser workflow -n workflow \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

```
UserReady=True (UserProvisioned)
DeliveryReady=True (Delivered)
Ready=True (AllReady)
```

If it does not converge, the reason names the step that holds it. The table
under [Conditions](../reference/c5c3/keystoneuser-crd.md#conditions) lists every
reason; `NamespaceNotAssigned` means Step 2 did not take effect.

### 5. Read the credentials

```bash
kubectl get keystoneuser workflow -n workflow \
  -o jsonpath='{.status.secretName}{" "}{.status.secretKeys}{"\n"}'
kubectl get secret workflow-credentials -n workflow \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

```yaml
clouds:
  "admin":
    auth:
      auth_url: "http://controlplane-keystone.openstack.svc:5000/v3"
      username: "workflow"
      password: "<generated>"
      user_domain_name: "Default"
    region_name: "RegionOne"
    endpoint_type: internal
    identity_api_version: 3
```

The document carries no project, so a token issued with it is unscoped. The
cloud entry is named after the ControlPlane's
`korc.adminCredential.cloudCredentialsRef.cloudName`; the credentials inside it
belong to the ordered user. A `password` key sits beside `clouds.yaml` for a
service that builds its own configuration.

The namespace holds no secret store, no client certificate and no ESO object:

```bash
kubectl get secretstore,certificate,pushsecret,externalsecret -n workflow
```

## Verification

Authenticate with the Secret from inside the namespace it was delivered to:

```bash
kubectl apply -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: workflow-verify
  namespace: workflow
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: osc
          image: ghcr.io/c5c3/tempest:2026.1
          env:
            - name: OS_CLIENT_CONFIG_FILE
              value: /etc/openstack/clouds.yaml
          command: ["/bin/sh", "-c"]
          args:
            - openstack --os-cloud admin token issue
          volumeMounts:
            - name: clouds
              mountPath: /etc/openstack
              readOnly: true
      volumes:
        - name: clouds
          secret:
            secretName: workflow-credentials
EOF

kubectl wait --for=condition=complete job/workflow-verify -n workflow --timeout=5m
kubectl logs -n workflow job/workflow-verify
kubectl delete job workflow-verify -n workflow
```

The log shows a token table without a project. A failing `token issue` means
the password in the Secret is not the one Keystone holds.

From the host, with the `OS_*` variables from the Quick Start's token-issue step
still exported, the user is visible from the admin's side:

```bash
openstack --insecure user show workflow
```

## Rotate the password

Raise the password generation, which may only increase:

```bash
kubectl patch keystoneuser workflow -n workflow --type merge \
  -p '{"spec":{"passwordGeneration":2}}'
kubectl get keystoneuser workflow -n workflow -o jsonpath='{.status.passwordGeneration}{"\n"}'
```

The operator generates a new password and K-ORC applies it in Keystone. While
it does, the Secret keeps the old password, which still authenticates. Once
`status.passwordGeneration` reads `2`, the Secret holding generation 1 is gone
from `openstack`. The new password goes to OpenBao first; once ESO has pushed
it, the Secret carries it and the order is `Ready` again. A service reads the
Secret again to pick the new password up.

## Order a project

The user has no project yet. Order one from the same namespace:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: KeystoneProject
metadata:
  name: workflow-project
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
EOF
kubectl wait --for=condition=Ready keystoneproject/workflow-project -n workflow --timeout=10m
kubectl get keystoneproject workflow-project -n workflow \
  -o jsonpath='{.status.projectName}{" "}{.status.domainName}{" "}{.status.projectID}{"\n"}'
```

The project name defaults to `metadata.name`, and the project lives in the
ControlPlane's admin domain, `Default` on the devstack. A project of that name
the order did not create, the admin project, or a built-in service project such
as `service-glance` is refused with `ProjectCollision`.

## Assign a role

A role assignment names the user and the project by their order names, and one
role:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: KeystoneRoleAssignment
metadata:
  name: workflow-member
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
  userRef:
    name: workflow
  projectRef:
    name: workflow-project
  role: member
EOF
kubectl get keystoneroleassignment workflow-member -n workflow \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

The entry of Step 2 lists no roles, so the order is refused, and the message
names the list and the role:

```
AssignmentReady=False (RoleNotAllowed)
Ready=False (NotAllReady)
```

Allow the role on the `workflow` entry. The `test` operation guards the index
as in [Withdrawing the assignment](#withdrawing-the-assignment-freezes-the-order):

```bash
i=$(kubectl get controlplane controlplane -n openstack -o json \
  | jq '[.spec.namespaceAssignments[] | .namespace == "workflow" and .targetClusterRef == null] | index(true)')
kubectl patch controlplane controlplane -n openstack --type json -p "[
  {\"op\":\"test\",\"path\":\"/spec/namespaceAssignments/${i}/namespace\",\"value\":\"workflow\"},
  {\"op\":\"add\",\"path\":\"/spec/namespaceAssignments/${i}/allowedRoles\",\"value\":[\"member\"]}]"
kubectl wait --for=condition=Ready keystoneroleassignment/workflow-member -n workflow --timeout=10m
kubectl get keystoneroleassignment workflow-member -n workflow \
  -o jsonpath='{.status.roleID}{" "}{.status.userID}{" "}{.status.projectID}{"\n"}'
```

The delivered `clouds.yaml` stays unscoped. A service scopes its token to the
project with two environment variables beside it. Rerun the Verification Job
with them:

```yaml
          env:
            - name: OS_CLIENT_CONFIG_FILE
              value: /etc/openstack/clouds.yaml
            - name: OS_PROJECT_NAME
              value: workflow-project
            - name: OS_PROJECT_DOMAIN_NAME
              value: Default
```

The token table now names the project. Taking the role off the list later
freezes the order and revokes nothing: the assignment stays in Keystone until
the order is deleted.

## Order an application credential

An application credential lets the service authenticate without the user's
password, and the operator replaces it on a schedule without an outage. The
operator creates it as the user, with a token scoped to the project, and that
token needs the role of the previous section. Order it for the user on the
project:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: KeystoneApplicationCredential
metadata:
  name: workflow-appcred
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
  userRef:
    name: workflow
  projectRef:
    name: workflow-project
EOF
kubectl wait --for=condition=Ready keystoneapplicationcredential/workflow-appcred -n workflow --timeout=15m
kubectl get keystoneapplicationcredential workflow-appcred -n workflow \
  -o jsonpath='{.status.credentialGeneration}{" "}{.status.credentialID}{" "}{.status.nextRotation}{"\n"}'
```

Without the role the order reports `CredentialReady=False/NoRoleOnProject` and
creates nothing. Once it is `Ready`, the Secret `workflow-appcred-credentials`
carries `clouds.yaml`, `application_credential_id` and
`application_credential_secret`:

```bash
kubectl get secret workflow-appcred-credentials -n workflow \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

```yaml
clouds:
  "admin":
    auth:
      auth_url: "http://controlplane-keystone.openstack.svc:5000/v3"
      application_credential_id: "<id>"
      application_credential_secret: "<generated>"
    auth_type: v3applicationcredential
    region_name: "RegionOne"
    endpoint_type: internal
    identity_api_version: 3
```

A token issued with this document is scoped to `workflow-project` and carries
the user's roles there, with no further variables. Rerun the Verification Job
with the Secret it mounts replaced:

```yaml
      volumes:
        - name: clouds
          secret:
            secretName: workflow-appcred-credentials
```

The token table names the project.

The schedule rotates the credential every 720 hours. The operator creates a
successor, switches the Secret once Keystone holds it, and deletes the
superseded credential 24 hours later, so the Secret never names an invalid
credential. During those 24 hours the status names the superseded one:

```bash
kubectl get keystoneapplicationcredential workflow-appcred -n workflow \
  -o jsonpath='{.status.previousCredentialID}{" "}{.status.previousCredentialDeleteAt}{"\n"}'
```

A service reads the Secret again within that grace period. Every credential
expires in Keystone at its creation time plus the interval plus the grace
period, so one the operator fails to delete stops working on its own.
`spec.rotation.interval` and `spec.rotation.gracePeriod` change the schedule;
the grace period stays shorter than the interval.

Rotate at once by raising `spec.credentialGeneration` above the generation the
status reports:

```bash
gen=$(kubectl get keystoneapplicationcredential workflow-appcred -n workflow \
  -o jsonpath='{.status.credentialGeneration}')
kubectl patch keystoneapplicationcredential workflow-appcred -n workflow --type merge \
  -p "{\"spec\":{\"credentialGeneration\":$((gen + 1))}}"
```

An interval of `0s` turns the schedule off, and `status.nextRotation`
disappears:

```bash
kubectl patch keystoneapplicationcredential workflow-appcred -n workflow --type merge \
  -p '{"spec":{"rotation":{"interval":"0s"}}}'
```

The live credential keeps its expiry and is rotated once it expires. Raise the
generation once more to replace it right away with a credential that does not
expire.

## Register a catalog entry

A catalog row is visible to every cloud user, so the entry has to admit catalog
entries on top of the assignment. Order the entry first:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: KeystoneCatalogEntry
metadata:
  name: workflow-dns
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
  serviceType: dns
  serviceName: designate
  endpoints:
    - interface: public
      url: https://dns.example.test/v2
EOF
kubectl get keystonecatalogentry workflow-dns -n workflow \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

```
CatalogReady=False (CatalogNotAllowed)
Ready=False (NotAllReady)
```

Set `allowCatalogEntries` on the `workflow` entry, with the index `i` from the
role step:

```bash
kubectl patch controlplane controlplane -n openstack --type json -p "[
  {\"op\":\"test\",\"path\":\"/spec/namespaceAssignments/${i}/namespace\",\"value\":\"workflow\"},
  {\"op\":\"add\",\"path\":\"/spec/namespaceAssignments/${i}/allowCatalogEntries\",\"value\":true}]"
kubectl wait --for=condition=Ready keystonecatalogentry/workflow-dns -n workflow --timeout=10m
openstack --insecure catalog show dns
```

The catalog lists the public URL in `RegionOne`. Every service type but
`identity` may be registered; a row of the same type and name that the order
did not create is refused with `ServiceCollision`. The endpoints may change
later, and an interface the spec no longer declares is removed. The type and
the name are frozen.

## Edit or delete the Secret

The operator owns the two keys it writes. Overwrite the password, then delete
the Secret, and watch it come back each time:

```bash
kubectl patch secret workflow-credentials -n workflow --type merge \
  -p '{"data":{"password":"Zm9v"}}'
kubectl get secret workflow-credentials -n workflow \
  -o jsonpath='{.data.password}' | base64 -d; echo
kubectl delete secret workflow-credentials -n workflow
kubectl get secret workflow-credentials -n workflow
```

Within seconds the password is the generated one again, and the deleted Secret
is back with an owner reference to the order. A key you add yourself is left in
place, because the operator owns only `password` and `clouds.yaml`.

## Withdrawing the assignment freezes the order

::: warning An assignment is consent, not a revocation tool
Removing the entry stops the operator from acting on the order. It revokes
nothing: the Keystone user keeps authenticating and the Secret stays until the
order is deleted.
:::

Look up the index of the `workflow` entry; the `test` operation rejects the
patch if another entry sits at that index by the time it applies:

```bash
i=$(kubectl get controlplane controlplane -n openstack -o json \
  | jq '[.spec.namespaceAssignments[] | .namespace == "workflow" and .targetClusterRef == null] | index(true)')
kubectl patch controlplane controlplane -n openstack --type json -p "[
  {\"op\":\"test\",\"path\":\"/spec/namespaceAssignments/${i}/namespace\",\"value\":\"workflow\"},
  {\"op\":\"remove\",\"path\":\"/spec/namespaceAssignments/${i}\"}]"
```

Within a minute both conditions report the gate, and the message names the
field that assigns the namespace again:

```
UserReady=False (NamespaceNotAssigned)
DeliveryReady=False (NamespaceNotAssigned)
Ready=False (NotAllReady)
```

The Secret and the Keystone user are still in place, and the operator no longer
repairs the Secret:

```bash
kubectl get secret workflow-credentials -n workflow
kubectl get users.openstack.k-orc.cloud -n openstack \
  -l c5c3.io/keystoneuser-name=workflow,c5c3.io/keystoneuser-namespace=workflow
```

Assign the namespace again with the Step 2 patch, and the order recovers:

```bash
kubectl wait --for=condition=Ready keystoneuser/workflow -n workflow --timeout=10m
```

## Revoking the user

Deleting the order is what revokes. Delete the application credential first,
then the role assignment. A user, a project or an assignment that a credential
order uses holds its deletion and reports `ReferencedByApplicationCredentials`,
and a user or a project that an assignment names reports
`ReferencedByRoleAssignments`, until the order that uses it is gone.

```bash
kubectl delete keystoneapplicationcredential workflow-appcred -n workflow
kubectl delete keystoneroleassignment workflow-member -n workflow
kubectl delete keystonecatalogentry workflow-dns -n workflow
kubectl delete keystoneproject workflow-project -n workflow
kubectl delete keystoneuser workflow -n workflow
```

K-ORC deletes the application credentials, unassigns the role, removes the
catalog rows and deletes the project before each order goes, and ESO removes
the credential's backup from OpenBao.

The last command blocks while K-ORC deletes the user from Keystone and ESO
removes the password from OpenBao. Afterwards the Secret is gone from `workflow` and
nothing labelled for the order is left in `openstack`:

```bash
kubectl get secret workflow-credentials -n workflow
kubectl get users.openstack.k-orc.cloud,secrets,pushsecrets -n openstack \
  -l c5c3.io/keystoneuser-name=workflow,c5c3.io/keystoneuser-namespace=workflow
```

The first command reports `NotFound` and the second reports no resources. See
[Deletion Semantics](../reference/c5c3/keystoneuser-crd.md#deletion-semantics).

## On a target cluster

The devstack runs one cluster, so this part cannot be followed on it. An owner
whose namespace lives on a registered target cluster orders the same way, on
that cluster:

1. The ControlPlane's entry names the cluster:
   `{"namespace":"workflow","targetClusterRef":{"name":"<cluster>"}}`.
2. The target cluster runs the `target-cluster-access` chart, which ships the
   KeystoneUser CRD, with `workflow` in `assignedNamespaces`, and the
   registration Secret's `namespaces` key lists it.
3. The ControlPlane publishes Keystone with
   `spec.services.keystone.publicEndpoint` or `spec.services.keystone.gateway`;
   without one the order reports `KeystoneNotPublished`.
4. The order is applied on the target cluster, with `controlPlaneRef.namespace:
   openstack`, and the Secret appears beside it there with the public URL.

See [Assigned namespaces](../reference/target-clusters.md#assigned-namespaces).

## Standalone Keystone, without a ControlPlane

There is no ordering path for a standalone Keystone. `spec.controlPlaneRef` is
required, and the order relies on what only a ControlPlane has: the admin
credential K-ORC authenticates with, the namespace assignments, and the secret
store the password is backed up through. On such an installation, create users
through the identity API directly.

## See also

- [KeystoneUser CRD: Consent](../reference/c5c3/keystoneuser-crd.md#consent): the assignment, the freeze, and orders on a target cluster.
- [KeystoneUser CRD: Delivered Secret contract](../reference/c5c3/keystoneuser-crd.md#delivered-secret-contract): the Secret's keys, the auth URL per cluster, and the repair.
- [KeystoneUser CRD: Conditions](../reference/c5c3/keystoneuser-crd.md#conditions): every reason the two conditions report.
- [KeystoneUser Reconciler Architecture](../reference/c5c3/keystoneuser-reconciler.md): the gates, the steps and the teardown order.
- [KeystoneProject CRD](../reference/c5c3/keystoneproject-crd.md), [KeystoneRoleAssignment CRD](../reference/c5c3/keystoneroleassignment-crd.md) and [KeystoneCatalogEntry CRD](../reference/c5c3/keystonecatalogentry-crd.md): the further orders, their consent and their conditions.
- [KeystoneApplicationCredential CRD](../reference/c5c3/keystoneapplicationcredential-crd.md): the rotation schedule, the Secret contract and the holds the credential order puts on the user, the project and the assignment.
- [Keystone Orders Reconciler Architecture](../reference/c5c3/keystone-orders-reconciler.md): the scaffold the five order kinds share, and the holds.
- [ControlPlane CRD: NamespaceAssignmentSpec](../reference/c5c3/controlplane-crd.md#namespaceassignmentspec): the assignment field and its validation.
- [Register a Service the ControlPlane Does Not Manage](./register-a-foreign-service.md): a KeystoneService with a catalog entry and roles.
- [ControlPlane E2E Test Suites](../reference/testing/controlplane-e2e-tests.md#keystone-user): the suite behind this guide.
- [Quick Start (ControlPlane)](../quick-start-controlplane.md): the devstack this guide builds on.

## Tested by

The flow above mirrors the following end-to-end suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/keystone-user
```

It orders a user from an assigned namespace and authenticates with the Secret
from a Job there, repairs an edited and a deleted Secret, and rotates the
password. It then orders a project, a role assignment and a catalog entry, sees
the role and the entry refused, admits both, scopes a token to the project and
reads the entry out of the catalog. It orders an application credential, sees
it refused before the role is assigned, authenticates with it, and observes a
scheduled rotation on a three-minute interval, the superseded credential
failing after its grace period, and a rotation by hand. It refuses an order
from an unassigned namespace, freezes every order by withdrawing the
assignment, restores it, and deletes the orders: the assignment first to see
it hold on the credential order, and the user to see it hold on the
assignment. Against a
full ControlPlane stack, run the suite with
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-controlplane`.

The suite brings up a Keystone-only ControlPlane of its own in chainsaw's
ephemeral namespace, so its fixtures are isolation-named where the walkthrough
is devstack-named: the plane is `cp` instead of `controlplane`, and the
`@TENANT_NS@` and `@CP_NS@` tokens are substituted per run so that parallel
suites never collide.

::: details The ControlPlane fixture the suite applies
<<< @/../tests/e2e/c5c3/keystone-user/00-controlplane-cr.yaml#controlplane
:::

::: details The order the suite applies from the assigned namespace
<<< @/../tests/e2e/c5c3/keystone-user/01-keystoneuser-tenant.yaml#keystoneuser-workflow
:::

::: details The project the suite orders
<<< @/../tests/e2e/c5c3/keystone-user/04-keystoneproject-tenant.yaml#keystoneproject-workflow-project
:::

::: details The role assignment the suite orders
<<< @/../tests/e2e/c5c3/keystone-user/05-keystoneroleassignment-tenant.yaml#keystoneroleassignment-workflow-member
:::

::: details The catalog entry the suite orders
<<< @/../tests/e2e/c5c3/keystone-user/06-keystonecatalogentry-tenant.yaml#keystonecatalogentry-workflow-dns
:::

::: details The application credential the suite orders
<<< @/../tests/e2e/c5c3/keystone-user/10-keystoneapplicationcredential-tenant.yaml#keystoneapplicationcredential-workflow-appcred
:::

::: details The Job the suite authenticates with the application credential from
<<< @/../tests/e2e/c5c3/keystone-user/11-openstack-appcred-verify-job.yaml#appcred-verify-job
:::
