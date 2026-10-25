---
title: Manage Provisioned Resources
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Manage Provisioned Resources

Provision identity, database and messaging resources for a service, then manage
its credentials and access. CobaltCore represents each request as an **Order**:
a Kubernetes custom resource in a namespace assigned to your service. The
ControlPlane provisions the requested resource and delivers any credentials as
a Secret in that namespace.

Orders cover Keystone users, projects, roles, catalog entries and application
credentials, plus MariaDB databases and RabbitMQ virtual hosts for messaging.
Use the OpenStack APIs to provision virtual machines and other cloud workloads.

For a complete walkthrough, start with
[Provision Resources](../quick-start-orders.md). This guide adds credential
rotation, access policies, recovery and deletion behavior. Its examples use a
service and namespace named `workflow`.

## Prerequisites

::: info Devstack
Complete [Deploy a ControlPlane](../quick-start-controlplane.md) and its
verification step. The examples assume `controlplane` is `Ready` in `openstack`,
with its Keystone instance running. Database and messaging requests also need
the shared MariaDB database and managed RabbitMQ bus.

```bash
EXTERNAL_CLUSTER=false KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```
:::

Run the commands from the repository root with `kubectl`, `jq` and the OpenStack
CLI available. Configure your shell using
[Connect to the ControlPlane](../quick-start-orders.md#shell) before running
`openstack` commands.

Database requests need OpenBao roles and policies. `make deploy-infra` creates
them automatically. When upgrading an older deployment, run these scripts
before requesting its first database. For the local environment, load the
bootstrap token and pass the ControlPlane's namespace and name:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-auth.sh
deploy/openbao/bootstrap/setup-policies.sh
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

For an existing OpenBao deployment, use its authorized bootstrap token instead.
Without this bootstrap the order reports `DatabaseReady=False/OpenBaoError`
naming the three scripts.

## Find resources and credentials {#background-what-lands-where}

| Description | Namespace | Why |
| --- | --- | --- |
| The namespace assignment | `openstack` | It is consent the ControlPlane gives, so it lives on the ControlPlane CR |
| The K-ORC User, its password Secrets, the source Secret and the PushSecret | `openstack` | K-ORC reads the admin credential there, and the PushSecret pushes the password to OpenBao through that namespace's own secret store |
| The order and the delivered Secret `workflow-credentials` | `workflow` | The credentials are delivered where the service that reads them runs |
| The K-ORC Project, Role import, RoleAssignment, Service, Region import and Endpoints of the further orders | `openstack` | K-ORC reads the admin credential there |
| The `KeystoneProject`, `KeystoneRoleAssignment` and `KeystoneCatalogEntry` orders | `workflow` | They live beside the user they belong to |
| The K-ORC ApplicationCredentials, their secrets, the user's `mint-cloud` document, the source Secret and the PushSecret of the application credential | `openstack` | K-ORC creates and deletes the credentials there, authenticated as the user |
| The `KeystoneApplicationCredential` order and its Secret `workflow-appcred-credentials` | `workflow` | The credential is delivered where the service that reads it runs |
| The ESO generator and ExternalSecret and the Database CR of the database order | `openstack` | OpenBao issues the credential through that namespace's identity, and the mariadb-operator creates the schema beside the MariaDB |
| The `MariaDBDatabase` order and its Secret `workflow-db-credentials` | `workflow` | The credential is delivered where the service that reads it runs |
| The topology operator's `Vhost`, `User` and `Permission` CRs, the password Secrets, the source Secret and the PushSecret of a vhost order | `openstack` | The RabbitMQ Messaging Topology Operator creates them on the broker that runs there, and the PushSecret pushes the credentials through that namespace's own store |
| The `RabbitMQVhost` order and its Secret `workflow-bus-credentials` | `workflow` | The credentials are delivered where the service that reads them runs |

The objects in `openstack` cannot carry an owner reference to an order in
another namespace, so they carry the labels `c5c3.io/keystoneuser-name`,
`c5c3.io/keystoneuser-namespace` and `c5c3.io/keystoneuser-cluster` instead.

The same resource and Secret placement applies when the service namespace is
on a target cluster:

![An order and its Secret on two clusters. On the management cluster the ControlPlane in the namespace openstack assigns the owner's namespace, and the c5c3-operator writes the order's children into openstack: the K-ORC User, Project, RoleAssignment, Endpoints and ApplicationCredential, the Database, the Vhost, User and Permission, the password and source Secrets, the PushSecret and the ExternalSecret with its generator, all owned by label because no owner reference crosses a namespace. K-ORC creates the pieces in Keystone, the mariadb-operator creates the schema in MariaDB, the topology operator creates the user on RabbitMQ, and OpenBao holds the backup every credential goes to before it is delivered. The delivered Secret sits beside the order in the owner's namespace with a controller owner reference and carries the in-cluster addresses. On a target cluster the access chart's ServiceAccount and Role let the c5c3-operator reach the owner's namespace over the registration Secret's kubeconfig; the children stay on the management cluster, and the Secret beside the order carries the published addresses instead.](../diagrams/controlplane-order-flow.svg)

## Create a service user {#steps}

### 1. Create the service's namespace

```bash
kubectl create namespace workflow
```

### 2. Allow resource requests from the namespace {#_2-assign-the-namespace-on-the-controlplane}

Authorize the `workflow` namespace to request resources. Use a JSON patch to
avoid replacing existing namespace assignments. If there are no assignments
yet:

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

### 3. Request a user {#_3-order-the-user}

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

### 4. Wait for the user to be ready {#_4-wait-for-the-order-to-converge}

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

If the user is not ready, inspect the condition reasons.
`NamespaceNotAssigned` means Step 2 did not take effect. See
[Conditions](../reference/c5c3/keystoneuser-crd.md#conditions) for other causes.

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

## Verify the credentials {#verification}

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

The log shows a token table without a project. If `token issue` fails, inspect
the Job logs for authentication, connectivity or TLS errors.

From the host, use the admin credentials configured in the prerequisites to
check the user:

```bash
openstack --insecure user show workflow
```

## Rotate the password

The operator rotates the password every 720 hours, the default of
`spec.rotation.interval`. The order reports its generation and when the next
rotation is due:

```bash
kubectl get keystoneuser workflow -n workflow \
  -o jsonpath='{.status.passwordGeneration}{" "}{.status.nextPasswordRotation}{"\n"}'
```

A Keystone user holds one password, so there is no grace period: the old
password stops authenticating the moment Keystone applies the new one. A
service reads the Secret again, or reconnects when it changes. Anything that
runs unattended belongs on an
[application credential](#order-an-application-credential), which rotates with
an overlap. For a consumer that cannot pick up a new password, set
`spec.rotation.interval` to `0s`, which turns the schedule off.

To rotate at once, raise the password generation above the one the status
reports. It may only increase:

```bash
next=$(( $(kubectl get keystoneuser workflow -n workflow -o jsonpath='{.status.passwordGeneration}') + 1 ))
kubectl patch keystoneuser workflow -n workflow --type merge \
  -p "{\"spec\":{\"passwordGeneration\":${next}}}"
kubectl get keystoneuser workflow -n workflow -o jsonpath='{.status.passwordGeneration}{"\n"}'
```

The operator generates a new password and K-ORC applies it in Keystone. The old
password stops working at that point, even if the delivered Secret still holds
it. The new password goes to OpenBao before delivery; once ESO has pushed it,
the Secret carries it and the order is `Ready` again. Consumers can briefly
fail authentication during this handover. `status.passwordGeneration` reports
the generation applied in Keystone, so also wait for `Ready` before using the
delivered password.
The schedule starts the same steps once `status.nextPasswordRotation` has
passed.

## Create a project {#order-a-project}

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

## Create an application credential {#order-an-application-credential}

An application credential lets the service authenticate without storing the
user's password. CobaltCore rotates it with an overlap, giving the service time
to read the new credential. Create it after assigning the user's project role:

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

The schedule rotates the credential every 720 hours, in three steps the figure
numbers:

![Rotation of an ordered credential as a time strip with two generations. Generation N is delivered from its mint. At the mint plus the interval the operator creates generation N+1 beside it, step 1, and the Secret still holds N. Once the backend holds N+1 the order switches to it, step 2: the status names N+1, the Secret is rewritten after the backup holds the new document, and the status names N as the previous credential. Both generations are valid until the grace period ends, step 3, when N is deleted. A credential created with the schedule on also expires in Keystone at its mint plus the interval plus the grace period, so one the operator fails to delete stops on its own. The schedule, a raised generation in the spec, an expired credential or a lost secret start a rotation; none starts while a grace period runs. The defaults are an interval of 720 hours and a grace period of 24 hours for the KeystoneApplicationCredential, where N+1 is a new credential of the same user, and for the RabbitMQVhost, where N+1 is a new broker user. Two kinds rotate differently: the KeystoneUser password rotates on the same default interval, replaces the password of the same user and has no grace period, so the old password stops when Keystone applies the new one, and the MariaDBDatabase credential follows the OpenBao lease on a cadence the order does not expose.](../diagrams/secrets-rotation-orders.svg)

1. The operator creates generation N+1 in Keystone. The delivered Secret keeps
   generation N, which stays valid.
2. Once Keystone holds N+1, the order switches: `status.credentialGeneration`
   reads N+1, `status.previousCredentialID` names N, and the Secret is
   rewritten as soon as the backup holds the new document.
3. When `status.previousCredentialDeleteAt` has passed, generation N is
   deleted.

The grace period between steps 2 and 3 is 24 hours, giving consumers time to
pick up the new Secret. Delivery and consumers must catch up before that
period ends. During it the status names the superseded credential:

```bash
kubectl get keystoneapplicationcredential workflow-appcred -n workflow \
  -o jsonpath='{.status.previousCredentialID}{" "}{.status.previousCredentialDeleteAt}{"\n"}'
```

A service reads the Secret again within that grace period. A credential minted
with scheduled rotation enabled expires in Keystone at its creation time plus
the interval plus the grace period, so one the operator fails to delete stops
working on its own.
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

## Set up messaging {#order-a-message-bus-vhost}

Give the service a RabbitMQ virtual host (vhost) and a user restricted to it.
The service can create queues and exchange messages within that vhost. This
request requires a managed RabbitMQ bus, such as the local environment's
`openstack-rabbitmq` (`messaging: {}`). Apply it in the assigned namespace:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: RabbitMQVhost
metadata:
  name: workflow-bus
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
EOF
kubectl wait --for=condition=Ready rabbitmqvhost/workflow-bus -n workflow --timeout=10m
kubectl get rabbitmqvhost workflow-bus -n workflow \
  -o jsonpath='{.status.vhost}{" "}{.status.username}{" "}{.status.passwordGeneration}{"\n"}'
```

The vhost is named after the order with a hash, `workflow-bus-<8 hex>`, and
the user after the vhost and the generation, `workflow-bus-<8 hex>-v1`. The
Secret `workflow-bus-credentials` carries `transport_url`, `host`, `port`,
`username`, `password` and `vhost`. Point oslo.messaging at the URL:

```bash
kubectl get secret workflow-bus-credentials -n workflow \
  -o jsonpath='{.data.transport_url}' | base64 -d
```

```text
rabbit://workflow-bus-<8 hex>-v1:<generated>@openstack-rabbitmq.openstack.svc:5672/workflow-bus-<8 hex>
```

The broker accepts the user and lists the vhost as its only permission. Check
both from the broker pod:

```bash
vhost=$(kubectl get rabbitmqvhost workflow-bus -n workflow -o jsonpath='{.status.vhost}')
user=$(kubectl get secret workflow-bus-credentials -n workflow -o jsonpath='{.data.username}' | base64 -d)
password=$(kubectl get secret workflow-bus-credentials -n workflow -o jsonpath='{.data.password}' | base64 -d)
printf '%s\n' "$password" | kubectl exec -i -n openstack openstack-rabbitmq-server-0 -c rabbitmq -- \
  sh -c 'IFS= read -r pw; rabbitmqctl authenticate_user "$1" "$pw"' _ "$user"
kubectl exec -n openstack openstack-rabbitmq-server-0 -c rabbitmq -- \
  rabbitmqctl list_user_permissions "$user"
```

The first command reports `Success`, and the second lists `$vhost` with `.*`
for configure, write and read. Passing the password through standard input
keeps it out of the `kubectl exec` arguments recorded in API audit logs.

The user rotates on the schedule the application credential follows: every 720
hours the operator creates the successor user `<vhost>-v2`, switches the Secret
once the broker holds it, and deletes the superseded user 24 hours later. The
three steps are the ones of [the figure above](#order-an-application-credential),
with a broker user in place of a credential.

The superseded user keeps working
through those 24 hours, so a service reads the Secret again within them.
`spec.rotation.interval` and `spec.rotation.gracePeriod` change the schedule,
`interval: 0s` turns it off, and raising `spec.passwordGeneration` above
`status.passwordGeneration` rotates at once.

Deleting the order deletes the user and its backup either way. The vhost and
the queues and messages in it stay under the default `spec.deletionPolicy:
Retain`, and an order re-created with the same name finds them again. Set
`Delete` before deleting the order to remove them as well:

```bash
kubectl patch rabbitmqvhost workflow-bus -n workflow --type merge \
  -p '{"spec":{"deletionPolicy":"Delete"}}'
kubectl delete rabbitmqvhost workflow-bus -n workflow
```

A retained vhost is removed by hand with `rabbitmqctl delete_vhost` in the
broker pod. See the [RabbitMQVhost CRD](../reference/c5c3/rabbitmqvhost-crd.md).

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

## Create a database {#order-a-database}

A `MariaDBDatabase` requests a database on the ControlPlane's shared MariaDB
and credentials restricted to it. Apply it after assigning the namespace in
Step 2:

```bash
kubectl apply -f - <<'EOF'
apiVersion: c5c3.io/v1alpha1
kind: MariaDBDatabase
metadata:
  name: workflow-db
  namespace: workflow
spec:
  controlPlaneRef:
    name: controlplane
    namespace: openstack
EOF
kubectl wait --for=condition=Ready mariadbdatabase/workflow-db -n workflow --timeout=10m
kubectl get mariadbdatabase workflow-db -n workflow \
  -o jsonpath='{.status.databaseName}{" "}{.status.roleName}{" "}{.status.secretKeys}{"\n"}'
```

The schema is `workflow_db`, the order's name with its dash as an underscore;
set `spec.databaseName` to name it yourself. The operator writes an OpenBao
role for the order, the mariadb-operator creates the schema, and ESO reads the
first credential from the role. The Secret `workflow-db-credentials` then
carries `host`, `port`, `database`, `username` and `password`, and `ca.crt`
when the ControlPlane's database runs TLS:

```bash
kubectl get secret workflow-db-credentials -n workflow \
  -o go-template='{{range $k, $v := .data}}{{if ne $k "password"}}{{$k}}={{$v | base64decode}}{{"\n"}}{{end}}{{end}}'
```

```
database=workflow_db
host=openstack-db.openstack.svc
port=3306
username=v-<generated by OpenBao>
```

The user holds `ALL PRIVILEGES` on `workflow_db` and nothing else. A Job in
`workflow` proves it: it logs in with the Secret's keys, writes a row and reads
it back, and fails if the login reaches the ControlPlane's `keystone` schema.
Apply the suite's Job from a checkout of the repository:

```bash
sed -e 's|@TENANT_NS@|workflow|g' tests/e2e/c5c3/keystone-user/13-mariadb-verify-job.yaml \
  | kubectl apply -f -
kubectl wait --for=condition=complete job/workflow-db-verify -n workflow --timeout=5m
kubectl logs -n workflow job/workflow-db-verify
kubectl delete job workflow-db-verify -n workflow
```

::: details The Job
<<< @/../tests/e2e/c5c3/keystone-user/13-mariadb-verify-job.yaml#mariadb-verify-job
:::

The credential rotates on its own. ESO reads a new user from OpenBao every 24
hours and the operator rewrites the Secret with it; OpenBao drops each user 48
hours after it issued it. A service therefore reads the Secret again at least
once a day, for example by mounting it as a volume and reconnecting when it
changes. `status.credentialsRefreshedAt` reports the last refresh.

The right half of the figure is this chain; the left half is the static mode
the ControlPlane's own services may still use.

![Database credentials in two modes. Static: a person writes username and password to an OpenBao KV path, an ExternalSecret copies them through the secret store into the Secret {cp}-{svc}-db-credentials, and MariaDB User and Grant resources create one long-lived SQL user from it. Dynamic: a VaultDynamicSecret generator logs in to OpenBao as the ServiceAccount {svc}-db-creds over a client certificate and draws a user from database/mariadb/creds/{svc}-{ns}, the database engine creates that user in MariaDB for one lease, and a changed credential rolls the Deployment through the db-connection-hash annotation. In both modes the service operator builds the DSN Secret that the pods read. A time strip shows the 24 hour refresh inside the 48 hour default TTL and the 72 hour maximum TTL.](../diagrams/secrets-db-credentials.svg)

The order refuses a ControlPlane whose shared database the OpenBao engine does
not issue for (External mode, a brownfield or dedicated Keystone database, or
`credentialsMode: Static`) with `DynamicCredentialsUnavailable`. A schema claimed by another Database
resource on the same MariaDB reports `DatabaseCollision`. The table under
[Conditions](../reference/c5c3/mariadbdatabase-crd.md#conditions) lists every
other reason the order reports.

`spec.deletionPolicy` decides what deleting the order does to the data. Under
`Retain`, the default, the schema and its data stay; under `Delete` the
mariadb-operator drops them. The user, its leases and the OpenBao role go
under both, so the last credential stops working the moment the order is
deleted:

```bash
kubectl patch mariadbdatabase workflow-db -n workflow --type merge \
  -p '{"spec":{"deletionPolicy":"Delete"}}'
kubectl delete mariadbdatabase workflow-db -n workflow
```

## Recover changed or deleted credentials {#edit-or-delete-the-secret}

CobaltCore restores the credential keys it manages. To verify recovery in this
tutorial environment, overwrite the password, then delete the Secret:

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

## Stop managing resources in a namespace {#withdrawing-the-assignment-freezes-the-order}

::: warning Existing access remains active
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

## Revoke access and remove resources {#revoking-the-user}

Delete Orders to revoke access. Remove application credentials first, followed
by role assignments, projects and users.

If another Order still references a resource, deletion waits with
`ReferencedByApplicationCredentials` or `ReferencedByRoleAssignments` until
you remove that dependency.

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

## Provision resources from another cluster {#on-a-target-cluster}

An owner whose namespace lives on a registered target cluster orders the same
way, on that cluster. The ControlPlane's entry names the cluster in
`targetClusterRef`, the `target-cluster-access` chart's `assignedNamespaces`
and the registration Secret's `assignedNamespaces` key name the namespace, and
the ControlPlane publishes the addresses of Keystone, the database and the bus
that a pod on that cluster can reach; without them the orders report
`KeystoneNotPublished`, `DatabaseNotPublished` or `MessagingNotPublished`.
[Provision Resources, Part 2](../quick-start-orders.md#target-cluster)
runs all of it on two kind clusters. See
[Assigned namespaces](../reference/target-clusters.md#assigned-namespaces).

On kind the published addresses need a path a real target cluster has by
itself. [The path from the target](../quick-start-orders.md#tc-path) names the
six hops.

![The path from a pod on the target cluster to the devstack in six numbered hops. Hop 1: the Job's pod resolves keystone.127-0-0-1.nip.io, and a CoreDNS rewrite on the target answers with the ClusterIP of the selector-less Service keystone-public in the owner's namespace. Hop 2: the pod sends the HTTPS request to that Service, keeping the hostname and with it the SNI and the Host header. Hop 3: the Service's EndpointSlice points at the devstack node's InternalIP on NodePort 31443, reached over the docker network kind. Hop 4: the Envoy proxy Service on the devstack terminates TLS with the Gateway's self-signed certificate, which the Job trusts through a ConfigMap copied by hand. Hop 5: the Gateway's listener for the hostname and the HTTPRoute hand the request to the Keystone Service on port 5000. Hop 6: the database and the bus are dialed at the node's IP and the NodePorts of two twin Services, which the ControlPlane publishes as its database and messaging endpoints, set by hand because the operator publishes nothing. Every part marked kind only stands in for what a production target cluster has: a DNS name that resolves, a CA, and published addresses a certificate carries.](../diagrams/controlplane-orders-target-path.svg)

## Use a standalone Keystone instance {#standalone-keystone-without-a-controlplane}

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
- [Keystone Orders Reconciler Architecture](../reference/c5c3/keystone-orders-reconciler.md): the scaffold the order kinds share, and the holds.
- [MariaDBDatabase CRD](../reference/c5c3/mariadbdatabase-crd.md) and [MariaDBDatabase Reconciler Architecture](../reference/c5c3/mariadbdatabase-reconciler.md): the database order, its rotation cadence, its deletion policies and the OpenBao role behind it.
- [RabbitMQVhost CRD](../reference/c5c3/rabbitmqvhost-crd.md) and [RabbitMQVhost Reconciler Architecture](../reference/c5c3/rabbitmqvhost-reconciler.md): the vhost order, its rotation, its deletion policy and its Secret contract.
- [ControlPlane CRD: NamespaceAssignmentSpec](../reference/c5c3/controlplane-crd.md#namespaceassignmentspec): the assignment field and its validation.
- [Register a Service the ControlPlane Does Not Manage](./register-a-foreign-service.md): a KeystoneService with a catalog entry and roles.
- [ControlPlane E2E Test Suites](../reference/testing/controlplane-e2e-tests.md#keystone-user): the suites behind this guide.
- [Deploy a ControlPlane](../quick-start-controlplane.md): the devstack this guide builds on.
- [Provision Resources](../quick-start-orders.md): the seven order kinds, one step each, on the devstack and on a target cluster.

## Tested by

The flow above mirrors the following end-to-end suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/keystone-user
```

The suite verifies user and project access, catalog entries, application
credentials and database requests. It also checks credential rotation, Secret
recovery, namespace authorization and deletion policies.

Against a full ControlPlane stack, run
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-controlplane`. The fixtures use a
ControlPlane named `cp` and substitute namespaces per run to avoid collisions.

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

::: details The database the suite orders
<<< @/../tests/e2e/c5c3/keystone-user/12-mariadbdatabase-tenant.yaml#mariadbdatabase-workflow-db
:::

The vhost order mirrors a second suite, the one with a live managed bus:

```bash
chainsaw test --test-dir tests/e2e/c5c3/messaging
```

The messaging suite checks vhost permissions, credential rotation, Secret
recovery and both deletion policies. It also verifies that withdrawing the
namespace assignment stops reconciliation. The `e2e-operator (c5c3)` CI job
runs this suite.

::: details The vhost order the messaging suite applies
<<< @/../tests/e2e/c5c3/messaging/01-rabbitmqvhost-tenant.yaml#rabbitmqvhost-workflow
:::
