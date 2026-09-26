---
title: ControlPlane E2E Test Suites
quadrant: operator
---

# ControlPlane E2E Test Suites

Reference documentation for the Chainsaw E2E test suites covering the
c5c3-operator's `ControlPlane` orchestration. These suites live in
`tests/e2e/c5c3/` and exercise the full ControlPlane → Keystone chain:
infrastructure projection, per-CR credential scoping in OpenBao, the K-ORC
application-credential handoff, catalog registration, deletion orchestration,
and multi-tenant isolation.

For the reconciler architecture and sub-reconciler contracts, see
[ControlPlane Reconciler](../c5c3/controlplane-reconciler.md). For the
Keystone-level suites, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The `tests/e2e/c5c3/` directory holds the ControlPlane suites. Each applies one
or more `ControlPlane` CRs (`c5c3.io/v1alpha1`) and asserts operator behaviour
end to end against the live cluster. The directory is the canonical inventory;
the table below is a guide, not a count.

Unlike the Keystone suites, the ControlPlane chain additionally requires K-ORC
and the c5c3-operator (on top of the keystone-operator, OpenBao, ESO, MariaDB,
and Memcached stack). The default kind E2E wiring does not install these, so
every suite follows the repo's **belt-and-braces presence-guard pattern**: a
runtime guard probes for the required CRDs, the OpenBao
`ClusterSecretStore`, and — for the suites whose ControlPlanes must actually
converge — **running keystone-operator pods**, exiting with a SKIP line when
any of it is absent. The pod probe matters because CRDs alone no longer imply
the stack: the `e2e-operator` c5c3 matrix leg installs every CRD the c5c3
controller watches (its informers cannot start otherwise) without deploying
the sibling operators.
Because Chainsaw has no step-level skip and the shared config runs with
`failFast: true`, the guard and all assertions live in a single script step.

Setting `E2E_REQUIRE_CONTROLPLANE_STACK=true` flips the guard from SKIP to a
hard failure. The dedicated `e2e-controlplane` CI job does exactly that: it
deploys keystone-operator, c5c3-operator, and K-ORC as local dev images
(`CONTROLPLANE_OPERATORS=external`), seeds the per-CR OpenBao paths
(`CONTROLPLANE_NAME=controlplane-keystone`), and runs the
`full-controlplane-keystone` suite so broken wiring in the live chain fails
the build instead of skipping. A second step on the same job runs
`keystone-service-foreign-namespace`, which brings up a Keystone-only
ControlPlane of its own and seeds that plane's OpenBao paths itself. A third
runs `keystone-service`, the own-namespace registration suite: the round-trip,
an injected K-ORC latch that holds for a misconfiguration and is cleared for a
transport failure, a rotation through a `CredentialRotation`, a collision held
until `adopt`, and deletion, against a Keystone-only plane of its own. The
three suites run in sequence because the shared chainsaw config sets
`failFast`, so one invocation over the directories would let a failure in any
abort the others. See the [CI workflow reference](../ci-cd/ci-workflow.md) for
the job definition.

## Running the Tests

```bash
# Bring up the full stack locally (kind)
WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external \
  CONTROLPLANE_NAME=controlplane-keystone make deploy-infra
hack/ci-deploy-korc.sh
OPERATOR=keystone IMAGE_REPO=<registry>/keystone-operator NAMESPACE=keystone-system hack/ci-deploy-operator.sh
OPERATOR=c5c3 IMAGE_REPO=<registry>/c5c3-operator NAMESPACE=c5c3-system hack/ci-deploy-operator.sh

# Run a single suite, failing loudly if the stack is missing
E2E_REQUIRE_CONTROLPLANE_STACK=true chainsaw test \
  --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/c5c3/full-controlplane-keystone/
```

Without the stack the suites skip cleanly, so `make e2e` (which runs the whole
`tests/e2e/` tree) stays safe on clusters that only carry the Keystone wiring.

## Test Suite Inventory

| Suite | CR Name(s) | Behaviour Validated |
| --- | --- | --- |
| [full-controlplane-keystone](#full-controlplane-keystone) | `controlplane-keystone` | The entire orchestration chain, link by link, through aggregate `Ready` and a live API check |
| [keystone-service-foreign-namespace](#keystone-service-foreign-namespace) | `cp` (ephemeral namespace) + `KeystoneService` `workflow` / `outsider` | Cross-namespace registration: an allowlisted namespace registers and authenticates with its consumer Secret, an unlisted one holds at `NamespaceNotAllowed`, and de-listing freezes instead of tearing down |
| [keystone-service](#keystone-service) | `cp` (ephemeral namespace) + `KeystoneService` `workflow` / `legacy` + `CredentialRotation` `rotate-workflow` | Own-namespace registration: the round-trip authenticates through the materialized clouds.yaml, an injected terminal K-ORC error holds the account at `ServiceAccountsFailed` for a 409 and is cleared for a transport failure, a CredentialRotation rotates the password, a registration colliding with pre-existing rows holds at `ServiceCollision` / `ServiceAccountCollision` until adopt takes them over, and deletion leaves no residue |
| [external-keystone](#external-keystone) | `controlplane-external` (+ 3 negative CRs) | External mode against a plain, operator-free Keystone: convergence with zero children, imports, the app-credential round-trip, no catalog pollution, a brownfield registration's round-trip, rotation and teardown, drift + rotation, `endpoint_type` detection, and zero-blast-radius deletion |
| [federated-controlplane](#federated-controlplane) | `controlplane-sso` | The end-user SSO experience: websso projection, the login page's SSO choice and domain field, the websso round trip through the gateway |
| [deletion-orchestration](#deletion-orchestration) | `deletion-orch` | ORC-teardown finalizer sequencing; deletion completes even when Keystone is already gone, and the projected Barbican registration and its label-owned K-ORC CRs leave no residue |
| [admin-password-scoping](#admin-password-scoping) | `controlplane` | Per-CR OpenBao-backed admin password projection |
| [db-credential-scoping](#db-credential-scoping) | `controlplane` | Per-CR OpenBao-backed service DB credential projection |
| [dedicated-backing-services](#dedicated-backing-services) | `cp` (ephemeral namespace) | Opt-in per-service dedicated database/cache: provisioning, ownership, sizing, and collective readiness gating |
| [sizing-profile](#sizing-profile) | `cp` (ephemeral namespace) + `SizingProfile` `sizing-<namespace>` | Sizing from a cluster-scoped `SizingProfile`: the managed backing services take the resolved sizing, a profile edit reaches them, and a deleted profile turns `SizingReady` False |
| [messaging](#messaging) | `cp` (ephemeral namespace) | The shared RabbitMQ bus: provisioned from `spec.infrastructure.messaging` with no service declared, owned, sized from the `Minimal` profile, gating `InfrastructureReady` on `AllReplicasReady`, and frozen against removal |
| [dedicated-namespaces](#dedicated-namespaces) | `cp` (ephemeral namespace) | Per-service dedicated namespaces: Managed/External lifecycles, backing-service placement, ownership labels, per-namespace tenant stores, and the deletion sweep |
| [multi-controlplane](#multi-controlplane) | `controlplane-a`, `controlplane-b` | Per-CR admin-credential isolation across two tenants; rotation non-interference |
| [secret-store-scoping](#secret-store-scoping) | — (namespace-only) | Per-ControlPlane OpenBao identity via a namespaced `SecretStore`; OpenBao-enforced cross-tenant isolation |
| invalid-keystoneservice-cr | multiple (rejected) | Every `KeystoneService` CEL `XValidation` and webhook rejection path pinned to a deterministic admission failure. Generated by its `_generate.py` and guarded by `make verify-invalid-cr-fixtures` |
| invalid-sizingprofile-cr | multiple (rejected, cluster-scoped) | Every `SizingProfile` marker and webhook rejection path pinned to a deterministic admission failure (fixtures `00` to `06`). Generated by its `_generate.py` and guarded by `make verify-invalid-cr-fixtures` |

## Test Suite Details

### full-controlplane-keystone

Applies one `ControlPlane` CR carrying eight services (keystone, horizon,
glance, placement, barbican, neutron, cinder, nova) and asserts the whole chain
link by link, gating each link on the previous one. It runs with
`spec.concurrent: false`, because the compute leg pins a node: the script labels
one node `openstack.c5c3.io/chassis=true` before it applies the ControlPlane, and
the `OVNChassis` pods that land there program the local Open vSwitch over host
paths any other chassis or metadata agent would share.

1. **Infrastructure** — owned MariaDB (`openstack-db`) and Memcached
   (`openstack-memcached`) created and owned by the ControlPlane;
   `InfrastructureReady=True`. The shared bus is referenced brownfield against a
   vhost of this suite's own on the kind broker, so no broker is projected: a
   projected broker would count against the 4 CPU / 16 GiB
   [node budget](#node-budget-link-6z) beside the kind broker the job already
   runs. `tests/e2e/cinder/broker-vhost.sh create` takes that vhost on
   `shared-rabbitmq` and renders its transport URL into the
   `controlplane-keystone-messaging` Secret; the suite's `finally` block calls
   the helper with `delete`, which tolerates a broker that is absent or
   restarting. A vhost of its own is what keeps these RPC topics out of the ones
   the parallel Cinder suites hold. The managed projection is what the
   [messaging](#messaging) suite proves.
   The suite then onboards the per-tenant OpenBao database-engine role
   (`setup-database-tenant.sh`), waits for `DBCredentialsReady=True`, and asserts
   the generator-backed ExternalSecret and engine-issued username.
2. **Keystone** — owned Keystone CR (`controlplane-keystone-keystone`) with
   the image tag derived from `spec.openStackRelease`, database/cache clusterRefs
   wired to the infra CRs, and `spec.database.credentialsMode: Dynamic`;
   `KeystoneReady=True`.
3. **ApplicationCredential** — owned K-ORC ApplicationCredential minted with
   `restricted: true`; `KORCReady=True`.
4. **Credential chain** — minted credential → operator Secret → PushSecret →
   OpenBao → operator-created per-CR `k-orc-clouds-yaml` ExternalSecret Ready;
   `AdminCredentialReady=True`.
5. **Catalog** — owned K-ORC Service and Endpoint, and the adopted Region
   (owner, `status.id`, description, `detach`, Available); `CatalogReady=True`.

5b. **Registrations** — one `KeystoneService` child per built-in service
   (`…-glance`, `…-placement`, `…-barbican`, `…-neutron`), each carrying that
   service's catalog entry and its service account: a managed User and Project,
   an unmanaged K-ORC Role import, and a managed RoleAssignment. Every child
   reports `Ready=AllReady`. The ControlPlane's `ServiceAccountsReady` is
   awaited later, in link 5m: it also aggregates the hypervisor operator's
   registration, which the Nova leg projects only once the Nova child is Ready.

5c. **Glance child** — owned Glance CR (`controlplane-keystone-glance`) with
   database/cache clusterRefs, an engine-issued (Dynamic) DB credential, the
   derived Keystone endpoint, the registered `glance` service user, and its
   `controlplane-keystone-glance-default` GlanceBackend against the Garage S3
   bucket; `GlanceReady=True`.

5d. **Image catalog** — owned K-ORC image Service
   (`controlplane-keystone-image-service`) plus an internal and a public
   Endpoint (`controlplane-keystone-image-endpoint-{internal,public}`). With no
   gateway in this fixture, both URLs advertise the in-cluster Glance API.

5e. **Placement child** — owned Placement CR
   (`controlplane-keystone-placement`) with database/cache clusterRefs wired to
   the infra CRs, the derived Keystone endpoint, the injected `placement`
   service user, and `spec.database.credentialsMode: Dynamic`;
   `PlacementReady=True`. Its DB credential is checked the way Keystone's is: a
   `controlplane-keystone-placement-db-credentials` ExternalSecret backed by a
   `VaultDynamicSecret` generator, with no static `data` refs, and a
   materialised Secret carrying an engine-issued username.

5f. **Placement catalog** — owned K-ORC placement Service
   (`controlplane-keystone-placement-service`) plus an internal and a public
   Endpoint (`controlplane-keystone-placement-endpoint-{internal,public}`).
   With no gateway in this fixture, both URLs advertise the in-cluster
   Placement API.

5g. **Barbican child** — owned Barbican CR (`controlplane-keystone-barbican`) on
   the Placement child's terms, plus the dedicated secret store the fixture asks
   for: an `OpenBaoCluster` reporting `Available=True`, a `BarbicanSecretStore`
   reporting `Ready=AllReady`, the AppRole Secret the barbican-operator mints
   against it, and `SecretStoresReady=AllStoresProjected` on the child;
   `BarbicanReady=True`.

5h. **Key-manager catalog** — owned K-ORC key-manager Service plus an internal
   and a public Endpoint, both advertising the in-cluster Barbican API.

5i. **OVN gate + Neutron child** — `OVNReady=True` with reason
   `OVNCentralReady`, mirroring the standalone `OVNCentral`
   (`controlplane-keystone-ovn`) the suite applies beside the ControlPlane and
   never owns. The `OVNChassis` on the labelled node follows, applied here rather
   than beside the compute leg because its pods register only against a serving
   OVN control plane, and Neutron binds a port only to a host carrying a live
   chassis row in the Southbound database. Then the owned Neutron CR
   (`controlplane-keystone-neutron`) on
   the Barbican child's terms: database/cache clusterRefs, an engine-issued
   (Dynamic) DB credential, the derived Keystone endpoint, and the registered
   `neutron` service user. On top of those it asserts the bus Secret
   `controlplane-keystone-neutron-messaging` the ControlPlane delivers and the
   child references brownfield, the `spec.ovn.centralRef` whose empty namespace
   resolved to the ControlPlane's own, and `spec.workers.deployment.replicas`
   taken from the `Minimal` profile; `NeutronReady=True`.

5j. **Network catalog** — owned K-ORC network Service plus an internal and a
   public Endpoint, both advertising the in-cluster Neutron API
   (`http://controlplane-keystone-neutron.openstack.svc:9696`).

5k. **Cinder child** — `CinderReady=True` over the owned Cinder CR
   (`controlplane-keystone-cinder`) on the Neutron child's terms:
   database/cache clusterRefs, the logical database `cinder`, an engine-issued
   (Dynamic) DB credential, the derived Keystone endpoint, the registered
   `cinder` service user, and the bus Secret
   `controlplane-keystone-cinder-messaging` it references brownfield. The DB
   credential is checked the way its peers are: a
   `controlplane-keystone-cinder-db-credentials` ExternalSecret backed by a
   `VaultDynamicSecret` reading `database/mariadb/creds/cinder-openstack`, no
   static `data` refs, a `cinder-db-creds` ServiceAccount, and a materialised
   Secret carrying an engine-issued username. On top of those it asserts what no
   peer carries: the `glanceEndpoint` and `keyManager.barbican.endpoint` of the
   sibling services, the `internalTenant` project and user IDs read off the
   registration's `status.account` rather than hardcoded, the API replica count
   taken from the `Minimal` profile, the two satellites `nfs1`
   (`CinderBackend`) and `nfsbk` (`CinderBackupBackend`) carrying the bare entry
   names and attached by `cinderRef`, and `status.volumeServices[0].host`
   reporting `controlplane-keystone-cinder@nfs1`, the host identity the volumes
   on that backend are keyed by.

5l. **Block-storage catalog** — owned K-ORC block-storage Service plus an
   internal and a public Endpoint. With no gateway in this fixture both
   advertise the in-cluster Cinder API on the project-less `/v3` path
   (`http://controlplane-keystone-cinder.openstack.svc:8776/v3`).

5m. **Nova child + its two DB chains** — `NovaReady=True` over the owned Nova CR
   (`controlplane-keystone-nova`), the widest projection of the chain: it needs
   the delivered bus Secret, the registration, two engine-issued DB credentials,
   the generated metadata shared secret, and the child's own Ready, which waits
   on the db-sync of both schemas and on five Deployments (API, metadata,
   scheduler, conductor, console proxy). The two database halves are asserted
   separately, each with its `clusterRef`, its logical name (`nova_api` and
   `nova`), its own `secretRef`, and `credentialsMode: Dynamic`; each chain is
   then checked the way its peers are, over the engine role and the ServiceAccount
   that buys its lease. On top of those the suite asserts what no peer carries:
   the metadata shared secret the ControlPlane generated (an ExternalSecret whose
   `dataFrom` names a `Password` generator, a `refreshInterval` of `0s`, and a
   value materialised under `shared_secret`), the compute contract Secret
   `controlplane-keystone-nova-compute-config` the child publishes on
   `status.computeConfigSecretRef`, and the four replica counts the ControlPlane
   sizes. With `NovaReady=True` the hypervisor operator's account
   (`hypervisorOperator: {}` in the fixture) has been provisioned too, so
   `ServiceAccountsReady` is awaited here, as `ServiceAccountsProvisioned` over
   every registration.

5n. **Compute catalog** — owned K-ORC compute Service plus an internal and a
   public Endpoint. With no gateway in this fixture both advertise the in-cluster
   Nova API on the `/v2.1` path the compute API is served under
   (`http://controlplane-keystone-nova.openstack.svc:8774/v2.1`).

5o. **The Neutron → Nova notifier leg** — the wire between the two services, and
   the one thing neither the Neutron nor the Nova link can assert alone. The
   Neutron child's `spec.nova.serviceUser` names `neutron-nova` and reads its
   password from the notifier registration's consumer Secret, and the
   `neutron-server` Deployment sources `OS_NOVA__PASSWORD` from that same Secret,
   which is where the wiring is visible since the password never reaches the
   rendered configuration. The registration behind it
   (`controlplane-keystone-neutron-nova`) reaches Ready carrying an account and
   no catalog entry, provisions the `neutron-nova` user, **references**
   `service-neutron` rather than creating it, and binds `service` **and**
   `admin`.

   The suite then applies `05-neutronmetadataagent-cr.yaml` on the chassis the
   OVN leg brought up and waits for it: the agent's `SecretsReady` gate resolves
   the shared secret the ControlPlane generated, so a Ready agent proves that
   value reaches a consumer outside the plane. The fixture names the Secret alone
   and leaves the key to the agent's defaulting webhook, the way an agent written
   by hand does, so a Ready agent also proves the generated key is the one both
   consumers default to. `06-fake-compute.yaml` follows, the nova-compute a compute
   cluster would build out of the contract Secret, and
   `tests/e2e/nova/discover-hosts.sh` runs the host discovery by hand:
   nova-scheduler places a server only on a host that also has a mapping in the
   API database, and nova-manage writes those from a 300-second periodic that is
   longer than the step can wait.

6. **Aggregate** — `Ready=True` with reason `AllReady`.

6z. **Node budget** — under `E2E_NODE_BUDGET=true`,
   `hack/ci-check-node-budget.sh` sums the effective requests of every pod on
   the kind node except the compute-node data plane and fails above 4000m CPU
   or 16Gi memory. Without the variable the link prints a SKIP line. See
   [Node budget (Link 6z)](#node-budget-link-6z).

6a. **Service status** — `status.services[]` reports eight entries, ready, in the
   order `setServicesStatus` emits them: keystone, horizon, glance, placement,
   barbican, neutron, cinder, nova.

6b. **Dynamic DB credential engine** — no static DB password remains at rest (the
   retired per-CR KV path is absent, AC 2/6); an engine-issued credential
   authenticates against MariaDB and is rejected after `bao lease revoke` (AC 3);
   and an unrelated lease survives another's revoke while the ControlPlane stays
   Ready (AC 4 single-tenant isolation).

7. **API reachable** — Keystone `/v3` returns HTTP 200, and a verify Job runs
   `openstack token issue` and `openstack catalog list` (using the `openstack`
   CLI bundled in the tempest image) against the materialised admin
   clouds.yaml, proving the minted, pushed, re-materialised application
   credential actually authenticates. The same Job greps the catalog for a
   placement row and calls `openstack resource class list` through the
   projected placement endpoint. That call and every round trip after it
   resolve their endpoints with the `region_name` the clouds.yaml pins, the
   lookup nova's client sections make, so each projected row has to sit in
   `spec.region`. It reads the region description back with `openstack --os-cloud
   admin region show RegionOne`, and the suite pins the Job's `OK: region
   description round-trip` line, so the value `spec.regionDescription` carries
   has to have reached Keystone. It closes with the network round trip:
   `network create cp-verify-net`, a `network show` that has to report
   `ACTIVE`, and a `network delete`. `cp-verify-net` is a logical network
   alone, written into the Northbound database by the northd running in the
   referenced central, so no chassis has to be bound for it. The block-storage
   half follows: the catalog carries a `block-storage` row, the `cinder` account
   holds `service` **and** `admin`, and four round-trips run through the
   projected Cinder endpoint. A 1 GiB volume on the `nfs1` backend reaches
   `available` (`OK: volume round-trip`). A backup of it is created, restored
   back into the same volume, and deleted (`OK: backup round-trip`). An
   encrypted volume type is created with a LUKS provider and a volume of that
   type counted into and back out of Barbican, so the key has to have reached
   the key manager and left it again with the volume (`OK: encrypted volume
   type`). Two volumes are created from one image: the first leaves a single
   `image-<id>` cache volume, and the second has to leave that count at one,
   since it clones the cached volume instead of pulling the image again
   (`OK: image-volume cache`). The internal tenant owns that volume, so the
   listing that counts it needs `--all-projects`. The suite greps the Job's logs
   for each of those four lines.

   The compute round-trip closes the Job. Exactly one nova-compute host has to
   report up, polled for a minute because the state travels over the bus on a
   periodic (`OK: compute service <node> up`). A server is then booted from a
   1 MiB raw image on an `m1.nano` flavor and a tenant network of the Job's own,
   and it has to reach `ACTIVE` on that same host
   (`OK: server cp-server ACTIVE on <node>`). Its port has to carry
   `vif_type: ovs` and a `host_id` naming the node, which is what tells a
   scheduled server from a networked one: a server whose port never bound still
   reads `ACTIVE` (`OK: port bound (ovs) on <node>`). The metadata request is
   then made by hand with the four headers a metadata agent sends, because the
   fake driver runs no guest: the correctly signed one has to return 200 with
   the server's own uuid, and one signed with a wrong secret has to return 403.
   That 403 is the half that proves the generated secret reached the compute
   side, since a metadata API configured with no secret at all answers 200 to
   both (`OK: metadata signed round-trip`). The server, the subnet, the network
   and the image are deleted again (`OK: compute round-trip`). The suite greps
   the Job's logs for each of those five lines, pinning the host to the node it
   labelled.

   Two log assertions follow the Job, on the two services the notifier wires
   together. The nova API has to have logged an `os-server-external-events` call
   answered `200`, which is the notifier reaching it; and the neutron-server log
   must carry no `network-changed … returned with failed status`, which is what a
   notifier account too narrow to resolve the instance would leave behind.

7d. **The hypervisor operator's account** — openstack-hypervisor-operator does
   not run in kind, so the suite proves what it needs from the control plane
   instead. The auth Secret `controlplane-keystone-nova-hypervisor-operator-auth`
   has to carry exactly the seven keys of the hypervisor operator's chart values;
   the script reads and prints the key names only, never the values, because
   the Secret carries a cloud-admin password. It then restarts K-ORC: the client
   K-ORC cached for the plane's `clouds.yaml` still holds the service catalog of
   a token issued before the `compute`, `network` and `block-storage` rows
   existed, and keeps it for up to 30 minutes. With a fresh cache it applies
   `fixtures.yaml` of the example overlay
   `deploy/kind/hypervisor-operator-fixtures/` (not its `image.yaml`, so CI
   downloads no cirros image) and waits up to 10 minutes for every K-ORC
   resource in that file to report `Available`.
   `07-hypervisor-operator-verify-job.yaml` follows, a Job that
   authenticates as the hypervisor operator's user from the auth Secret and
   prints one line per block, which the suite greps:
   - `OK: hvo catalog`: the account's token, and one scoped to `test@cc3test`,
     each carry exactly one public `compute` and one public `placement` row.
   - `OK: hvo compute services`: it enables and forces up the fake compute's
     service, lists its hypervisor and the servers on it across projects.
   - `OK: hvo aggregates`: it creates an aggregate, adds and removes the host,
     and deletes the aggregate.
   - `OK: hvo placement`: it reads the host's resource provider and its traits,
     then creates a trait and sets it on a scratch provider, which it deletes.
   - `OK: hvo test scope`: in `test@cc3test` it issues a token and finds flavor
     `1`, the public volume type `premium` and the non-shared network
     `hvo-smoke-test`.

   Neither live nor cold migration is called: the suite has one compute, and
   both check the same admin rule the services and aggregates blocks prove.

The block-storage service widens this suite's presence guard. Beyond the CRDs
and operators its peers need, it requires a running cinder-operator, a
`shared-rabbitmq` broker reporting `AllReplicasReady`, and an `nfs-server`
Deployment in `openstack`: Cinder casts real RPC over the bus and mounts both of
its shares from the in-cluster export, so a cluster missing either stack brings
up no Cinder at all. `hack/deploy-infra.sh` installs neither by default, so the
cluster has to be brought up with `WITH_MESSAGING=true` and `WITH_NFS=true`,
which is what the `e2e-controlplane` job sets. Probing for them turns a missing
stack into one gated line instead of a `CinderReady` wait that burns the script
budget. The compute service widens the guard once more: it needs a running
nova-operator, and the chassis its server's port binds on needs the
`openvswitch` and `geneve` kernel modules on the runner host, which the
`e2e-controlplane` job asks for with `WITH_OVN_KERNEL_MODULES: "true"`.

The suite budgets 65 minutes for the consolidated script, 5 for its diagnostic
`catch`, and one timeout per `finally` script: 5 minutes to delete the verify
Job, 2 to release the broker vhost, and 5 for the compute leg's own footprint.
That last one first deletes the hypervisor-operator Job and the K-ORC fixtures,
while the ControlPlane's credential K-ORC deletes them through still works. It
disables the `cc3test` domain before that delete, since Keystone refuses to
delete an enabled domain and K-ORC does not disable one first. It then deletes
the fake compute, the metadata agent and the chassis in reverse order of the
apply and takes the `openstack.c5c3.io/chassis` label off every node, because
none of the three is owned by the ControlPlane and a label left standing
advertises a chassis role nothing serves. With the shared
3-minute cleanup budget the suite's ceiling is
`65 + 5 + 5 + 2 + 5 + 3 = 85` minutes. Every one of those timeouts is pinned
rather than inherited: chainsaw applies the suite's `exec` budget to each script
operation, so an unpinned `catch` or `finally` would each carry 30 minutes of
their own.

#### Node budget (Link 6z)

The full ControlPlane stack on the `Minimal` sizing profile has to fit one
4 vCPU / 16 GiB node. The kind node of the self-hosted runner is larger than
that, so a stack that outgrows the target would still schedule there. Link 6z
turns the target into a check. Right after the aggregate `Ready=True` of
Link 6, the suite runs `hack/ci-check-node-budget.sh`, which reads the cluster
and changes nothing:

- It expects exactly one node and exits 2 otherwise.
- It lists every pod on that node that is neither `Succeeded` nor `Failed`,
  across all namespaces.
- It computes each pod's effective request per resource with the scheduler's
  rule: the larger of the container sum plus the `restartPolicy: Always` init
  containers and the largest other init container, which counts together with
  the `restartPolicy: Always` init containers declared before it, plus
  `spec.overhead`. A missing request counts as 0.
- It prints one row per pod (`NAMESPACE/POD`, CPU in millicores, memory in
  MiB, sorted by CPU), then a `TOTAL` and a `BUDGET` row.
- It exits 0 within budget, and 1 with one
  `ci-check-node-budget: over budget: cpu <total>m > <budget>m` or
  `… memory <total>Mi > <budget>Mi` line per exceeded resource. It exits 2 when
  it cannot measure: a failing `kubectl`, an unparsable quantity, or no `jq`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `NODE_BUDGET_CPU` | `4000m` | CPU budget, a Kubernetes quantity |
| `NODE_BUDGET_MEMORY` | `16Gi` | Memory budget, a Kubernetes quantity |
| `NODE_BUDGET_SELECTOR` | empty (count every pod) | Label selector passed to `kubectl get pods -l`; only the pods it matches count |

The suite sets `NODE_BUDGET_SELECTOR` to
`app.kubernetes.io/name notin (ovnchassis,neutronmetadataagent,nova-fake-compute)`.
Those are the OVN chassis, the metadata agent and the fake compute of the
compute leg. They stand in for a hypervisor, which lies outside a budget for
the control plane, its backing services, the operators and the platform stack.
Everything else counts, including `kube-system`, the OVN central,
the kind NFS export, the shared RabbitMQ broker and K-ORC.

The link enforces the budget only when `E2E_NODE_BUDGET=true`, which the
`e2e-controlplane` job sets on the full-chain step. That job also exports
`OPERATOR_REPLICAS=1`, so every operator runs one replica, as in the kind
devstack. Without the variable the link prints
`SKIP: node budget gate (set E2E_NODE_BUDGET=true to enforce)`, so a local run
on a busier cluster does not fail. To enforce it against a local stack, run
the suite with the variable set, or call the script directly:

```bash
NODE_BUDGET_SELECTOR='app.kubernetes.io/name notin (ovnchassis,neutronmetadataagent,nova-fake-compute)' \
  hack/ci-check-node-budget.sh
```

The gate proves that the requests fit. The larger CI node does not reproduce
the CPU contention of a real 4 vCPU host.

### external-keystone

Proves the External-mode adoption contract against a Keystone the operator does
**not** own. The suite stands up a plain, operator-free Keystone fixture
(`00-fixture-keystone.yaml`, a single SQLite-backed pod with its own bootstrap
history and admin Secret) in the `brownfield-keystone` namespace and populates
its catalog with a non-default admin identity (domain `heimdall`, project
`platform-admin`, user `brownfield-admin`) and a duplicate identity service
(`01-fixture-catalog-setup-job.yaml`). It then drives four External
`ControlPlane`s against that fixture and asserts, in one consolidated script:

1. **Converge** — the main External CR reaches `Ready=True/AllReady` with **zero**
   MariaDB/Memcached/Keystone/Horizon children; the skipped sub-reconcilers report
   `ExternallyManaged` and Horizon reports `HorizonNotManaged`.
2. **Imports resolve** — `KORCReady=ApplicationCredentialMinted`,
   `CatalogReady=CatalogImported`, and `status.catalog.imports` shows the identity
   Service plus three Endpoint interfaces resolved with OpenStack ids.
3. **App credential** — minted against the external Keystone, present at the per-CR
   OpenBao path with the ESO `managed-by` stamp and in a materialised clouds.yaml
   targeting the external API; a verify Job authenticates a client with it.
4. **No pollution** — once the ControlPlane has converged, the external services,
   endpoints, domains, roles, users and projects are byte-identical to a
   pre-recorded baseline.
5. **Brownfield registration** — the `nova` `KeystoneService` (a `compute` catalog
   entry with two endpoint rows, an account in `heimdall` holding the `service`
   role) reaches `Ready=True/AllReady`. Its consumer Secret `nova-credentials`
   authenticates a verify Job against the external API, and `catalog show nova`
   carries the registered row. The admin-side inventory gained the service, the
   two endpoints, the user and the project the registration declared, and nothing
   else. A `CredentialRotation` then rotates the account password: the new one
   authenticates, the old one is rejected, and the verify Job re-runs on the
   rotated Secret. Its teardown takes those rows back out again.
6. **Drift + rotation** — changing the external admin password without updating the
   Secret makes a forced re-mint fail loudly (a documented drift reason, no
   remediation); updating the Secret then drives a hash-driven re-mint to a fresh
   credential, and the old application credential is invalid.
7. **endpoint_type detection** — a ControlPlane pinned to an unreachable internal
   interface fails loudly (never a silent-empty import), while `public` converges.
8. **Negative paths** — a wrong password yields a distinct `AuthenticationFailed`
   condition; an ambiguous identity catalog fails with `CatalogFailed`.
9. **Zero blast radius** — deleting the main CP revokes the app credential, removes
   the OpenBao-backed Secrets, emits `ORCTeardownComplete`, leaves the external
   users/domains/catalog bit-for-bit identical to the baseline, and the fixture
   keeps serving tokens.

Run it locally against a full ControlPlane stack with
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-external-keystone`. It has its own
dedicated `e2e-external-keystone` CI job (see the
[CI workflow reference](../ci-cd/ci-workflow.md)).

### deletion-orchestration

Covers the `c5c3.io/orc-teardown` finalizer. Drives a ControlPlane to Ready,
initiates deletion, then deletes the projected Keystone CR so K-ORC can no
longer revoke the admin credential against a live API. Asserts that
ControlPlane deletion still **completes** within a window larger than the
bounded stall deadline (`orcTeardownDeadline`, 7m): the finalizer waits,
then force-removes the stuck `openstack.k-orc.cloud/*` finalizers. Also
asserts the projected Keystone, MariaDB, Memcached, and all six K-ORC CRs are
garbage-collected and an ORC-teardown event (`ORCTeardownComplete`, or the
Warning `ORCTeardownStalled` on the stalled path) was emitted. Before the
teardown it checks that the projected `deletion-orch-barbican` `KeystoneService`
and at least one K-ORC CR carrying its `c5c3.io/keystoneservice-name` label are
standing. Once the ControlPlane is gone it asserts that the `KeystoneService`
and every K-ORC CR carrying that label are gone, which is what the force-release
on the Keystone-gone path produces.

### admin-password-scoping

Asserts that `reconcileAdminPassword` projects a per-ControlPlane,
OpenBao-backed admin password: an owned ExternalSecret
`controlplane-keystone-admin-credentials` whose `password` remoteRef reads the
per-CR OpenBao key `bootstrap/{namespace}/{keystoneName}/admin`, plus the
materialised Secret of the same name. The path is keystone-name scoped so it
matches the keystone-operator's scheduled admin-password rotation PushSecret,
which reads and writes the same key.

### db-credential-scoping

Onboards the per-tenant OpenBao database-engine role
(`setup-database-tenant.sh`) and asserts that `reconcileDBCredentials` projects a
per-ControlPlane, DYNAMIC (engine-issued) DB credential: a `VaultDynamicSecret`
generator reading `database/mariadb/creds/keystone-{namespace}`, an owned
`ExternalSecret` `controlplane-keystone-db-credentials` drawing from that
generator via `dataFrom.sourceRef.generatorRef` (no static Data refs), a
`keystone-db-creds` ServiceAccount, and a materialised Secret carrying an
engine-issued username (not the static `keystone` user). The stage-(a) static
per-CR KV seed is retired (#439).

### dedicated-backing-services

Asserts the opt-in per-service
[dedicated backing services](../c5c3/controlplane-crd.md#dedicatedbackingservices):
a `ControlPlane` whose Keystone service takes a dedicated database **and** cache
and whose Horizon dashboard takes a dedicated cache. It proves a dedicated
instance carries the shared block's lifecycle — it is provisioned as a `MariaDB` /
`Memcached` child, owned with a **controller owner reference and
`blockOwnerDeletion`** (the mechanism that tears it down with the ControlPlane),
sized from its **own** `replicas` / `storageSize`, and gates `InfrastructureReady`
so the consuming service waits for the database it actually talks to.

The fixture's **shared** block is brownfield, so the ControlPlane provisions
nothing for it: the exact set of `MariaDB` / `Memcached` CRs in the namespace *is*
the dedicated set, which the suite asserts as an exact set rather than a superset
— the proof that a service which opted out no longer gets the shared instance.

Unlike the sibling suites it runs in **chainsaw's ephemeral namespace** with a
ControlPlane of its own (`cp`) rather than reusing the canonical `openstack` one.
Two contracts rule that out: the webhook permits one ControlPlane per namespace,
and the shared↔dedicated presence flip is frozen on a live CR, so the dedicated
declaration cannot be patched onto the pre-existing shared ControlPlane — it has
to be created with it.

The suite's presence guard probes every CRD the ControlPlane controller
watches (the Keystone, KeystoneIdentityBackend, Horizon, and K-ORC kinds
alongside the ones asserted): a controller-runtime informer for a kind whose
CRD is absent never syncs, so on a cluster missing any of them the operator's
elected leader dies on the cache-sync timeout and the reconciler this suite
drives never runs at all.

The projected-child assertions (the Keystone child pointing at the dedicated
instances, with `credentialsMode: Static`) are deliberately not part of the
suite: reaching the Keystone projection requires the DB-credential and
admin-password machinery to converge, which needs OpenBao seeded for the
ControlPlane's namespace — and this suite runs in an ephemeral namespace by
design. They are hard-asserted in the envtest scenario
`TestIntegration_DedicatedBackingServices`, which runs against the real CRD
schema and webhook on every PR.

### sizing-profile

Asserts sizing from a cluster-scoped
[`SizingProfile`](../c5c3/controlplane-crd.md#sizingprofile). The script
creates a profile of base `Minimal` named `sizing-<namespace>` and a
`ControlPlane` that references it and gives Keystone a dedicated managed
database and cache stating no replica count and no volume size. It asserts that:

- the defaulting webhook stored the profile's database sizing (`replicas: 1`,
  `storageSize: 512Mi`) on the dedicated database block;
- the dedicated MariaDB runs one replica with `Minimal`'s 1Gi memory limit, the
  dedicated Memcached takes the 128Mi one, and `SizingReady` is `True`;
- patching the profile's `cache.resources` to a 192Mi limit reaches the
  Memcached within 60 s, through the watch the controller keeps on the kind;
- deleting the profile turns `SizingReady` `False` with reason
  `SizingProfileNotFound`.

It follows the dedicated-backing-services suite: an ephemeral namespace, a
brownfield shared block that provisions nothing, and one guarded script step
whose presence guard also probes `sizingprofiles.c5c3.io`. The profile is
cluster-scoped, so the namespace teardown never reaps it; the script deletes the
ControlPlane first and the profile last on every exit path.

### messaging

Asserts the shared
[RabbitMQ message bus](../c5c3/controlplane-crd.md#messagingspec): a
`ControlPlane` that declares `spec.infrastructure.messaging` in managed mode
(`clusterRef: cp-rabbitmq`) and **no service at all**
(`services: {}`). A broker that appears can only come from the messaging block
itself, which is the distinction the suite exists to pin: a database and a cache
follow the services that consume them, the bus does not.

The fixture's shared database and cache are **brownfield**, addressing
`.invalid` endpoints nothing dials. A brownfield block provisions nothing, so the
`RabbitmqCluster` is the only backing-service instance in the namespace and the
suite asserts that as an **exact set** (no `MariaDB`, no `Memcached`).

What it checks on a live cluster:

- the `RabbitmqCluster` `cp-rabbitmq` exists;
- it carries a **controller owner reference with `blockOwnerDeletion`** from the
  ControlPlane, the teardown contract the sibling dedicated-backing-services
  suite pins for MariaDB and Memcached;
- its `spec.replicas` is `1`, taken from the fixture's `Minimal` profile;
- it reaches `AllReplicasReady=True` within 600s. The RabbitMQ Cluster Operator
  publishes no `Ready` condition, so this is the condition the reconciler gates
  on as well;
- `InfrastructureReady` then flips to `True` with reason `InfrastructureReady`
  within 180s;
- dropping the **managed** messaging block from the live ControlPlane is
  **rejected** by the webhook. This is the managed half of the one-way add, and
  this is the only suite with a live managed bus to assert it against; the
  brownfield half (also *rejected*, so the mode freeze cannot be laundered into a
  two-step flip) lives in the `invalid-cr` messaging-freeze wave.
- deleting the ControlPlane tears the broker down through the operator's own
  path: the `RabbitmqCluster` goes with its owner, the operator labels the pod
  `skipPreStopChecks`, and no broker pod remains. The suite does this itself
  instead of leaving it to chainsaw's namespace cleanup. On Kubernetes >= 1.33
  the namespace controller deletes pods first and revisits the namespace only
  after half the longest pod `terminationGracePeriodSeconds` it found, which is
  about 3.5 days for a broker pod at the operator's default of 604800s, so a
  namespace deleted around a live broker wedges with the ControlPlane never
  deleted.

It runs in **chainsaw's ephemeral namespace** with a ControlPlane of its own
(`cp`) for the reasons the dedicated-backing-services suite gives, plus one of
its own: managed messaging is a one-way add on a live CR, so the declaration
cannot be patched onto the canonical `openstack` ControlPlane and then unwound.

The presence guard probes the CRD set the sibling suite probes plus
`rabbitmqclusters.rabbitmq.com`, and SKIPs when any is absent
(`E2E_REQUIRE_CONTROLPLANE_STACK=true` turns the SKIP into a hard failure). The
RabbitMQ leg earns its place: the c5c3 operator watches that kind behind a
discovery gate, so a cluster without the rabbitmq-cluster-operator admits the
fixture and then parks the ControlPlane at `InfrastructureReady=False` with
reason `RabbitMQError`. Probing the CRD turns that into a SKIP instead of a
doomed wait for a broker the cluster can never create.

Two things are left unasserted on purpose. **Consumer wiring** is one: no service
reads the bus yet, so the suite stops at the provisioned, owned, sized, ready
broker, and the transport-URL projection into a service's oslo.messaging config
lands with the first consumer. **Pod resources** are the other: the `Minimal`
profile sizes the broker at 100m CPU and 512Mi per pod, limited at 512Mi,
instead of the operator's default of 1 CPU and 2Gi, and the PVC stays at the
operator's 10Gi. A pod that will not schedule on the CI node is a finding to
report; the fixture stays as it is.

### dedicated-namespaces

Asserts per-service
[dedicated namespaces](../c5c3/controlplane-crd.md#service-namespaces): a
`ControlPlane` that places its Keystone service in an operator-owned (`Managed`)
namespace and its Horizon dashboard in a pre-existing (`External`) one. It proves
the placement and lifecycle contract on a live cluster:

- `NamespacesReady` goes `True` once the `External` namespace (pre-created by the
  test) is present and the `Managed` one has been created by the operator;
- the `Managed` namespace carries the ownership labels plus
  `app.kubernetes.io/managed-by`; the `External` namespace is left **unlabelled**;
- the one shared `spec.infrastructure` block materializes its backing services in
  **each service's** namespace — a `MariaDB` and `Memcached` in the Keystone
  namespace, a `Memcached` in the Horizon namespace — each carrying the ownership
  labels and **no owner reference** (Kubernetes forbids a cross-namespace one),
  and nothing in the ControlPlane's own namespace;
- a per-tenant `openbao-tenant-store` `SecretStore` is provisioned in every
  namespace the ControlPlane occupies;
- a `KeystoneService` placed in the `Managed` Keystone namespace is admitted
  without an allowlist entry and parks at
  `AccountReady=False/WaitingForAdminCredential`, since the suite never seeds
  OpenBao; `CatalogReady` reads `True/CatalogNotDeclared`, no consumer Secret is
  delivered into that namespace and no K-ORC child is projected for it, and the CR
  is released within 180 s of the ControlPlane's deletion;
- on deletion the cross-namespace children are torn down explicitly (no GC
  cascade reaches them), the `Managed` namespace is deleted, and the `External`
  namespace **survives** with its ControlPlane residue swept.

Like the sibling dedicated-backing-services suite it runs in **chainsaw's
ephemeral namespace** with a ControlPlane of its own, deriving the two service
namespaces from it: the namespace-assignment fields are frozen after creation and
a service namespace is a tenant key admission reserves to one ControlPlane, so the
suite cannot reuse the canonical `openstack` ControlPlane. It carries the same CRD
presence guard and `E2E_REQUIRE_CONTROLPLANE_STACK` escalation.

The credential material and the projected Keystone child sit behind the
OpenBao-seeded DB-credential / admin-password machinery this ephemeral suite
cannot reach; the full cross-namespace readiness (the credential ExternalSecrets,
the projected child, the OpenBao path re-keying) is hard-asserted in the envtest
scenario `TestIntegration_DedicatedNamespaces` on every PR. The registration's
credential delivery into the dedicated namespace waits on the same unseeded admin
credential, and that scenario asserts it too.

### multi-controlplane

Brings up two ControlPlanes in two namespaces (`tenant-a/controlplane-a`,
`tenant-b/controlplane-b`), onboards each tenant's distinct database-engine role,
and asserts admin-credential isolation (each CR's minted admin application
credential lands on a distinct per-CR path with different material; rotating only
tenant-a's credential leaves tenant-b unchanged) **and** dynamic DB-credential
isolation: the two tenants draw from distinct per-tenant roles, and revoking
tenant-a's DB leases by prefix leaves tenant-b's credential authenticating and
tenant-b Ready (AC 4).

### secret-store-scoping

Exercises the half of the per-ControlPlane secret-store feature (#605) that unit
and integration tests cannot reach: the **live OpenBao identity** a ControlPlane
gets through a namespaced `SecretStore`, and OpenBao's own enforcement of
cross-tenant isolation. Running in the ephemeral test namespace, the suite:

1. runs `setup-eso-tenant.sh <namespace>`, which provisions the tenant
   `ServiceAccount` (`eso-tenant-auth`), the cert-manager mTLS `Certificate`, and
   the namespaced `SecretStore` (`openbao-tenant-store`);
2. asserts that `SecretStore` reaches `Ready=True` — proving the `eso-tenant`
   auth role, the `eso-tenant` templated policy, and mTLS actually authenticate
   the per-tenant identity against OpenBao;
3. mints a token from the tenant's `eso-tenant-auth` ServiceAccount, logs in as
   the `eso-tenant` role, and proves the token can read **its own** namespace's
   Keystone key path but is **denied** on a foreign namespace's path — the
   templated-policy isolation that replaces the naming convention;
4. logs in as the shared `eso-management` role and proves it is **denied** both
   read and write on a Keystone key path (#606 retired the `push-*` write
   policies and dropped `eso-management`'s `openstack/keystone/*` read) while
   still reading the retained shared `bootstrap/*` subtree;
5. applies an `ExternalSecret` referencing the shared `openbao-cluster-store`
   from the non-allow-listed ephemeral namespace and proves it never goes
   `Ready`, because #606 restricted the cluster store with `spec.conditions`;
6. drives the **never-seeded first-push** round-trip on a per-CP app-credential
   path (`openstack/keystone/{ns}/{cp}/admin/app-credential`, the shape the
   External-mode round-trip uses) through the operator-default tenant store:
   the first `PushSecret` creates the leaf and ESO stamps
   `managed-by=external-secrets` itself (the managed-by guard's inverse), a
   read-back `ExternalSecret` materialises the exact value, and
   `DeletionPolicy: Delete` purges the leaf via the `eso-tenant` delete grant —
   with zero seeded state and nothing per-CP beyond the one-time cluster
   bootstrap.

The ControlPlane→Keystone/Horizon projection and the `SecretsReady` gating are
covered by the c5c3 operator integration test
(`TestIntegration_SecretStoreRefProjectedAndGated`), so this suite focuses on the
behaviour only a live OpenBao can prove. It SKIPs cleanly when the stack — or the
`eso-tenant` role (bootstrap predating #605) — is absent.

### keystone-service-foreign-namespace

Exercises the one thing the `KeystoneService` CRD exists for and no other suite
reaches: registering a service from a namespace the ControlPlane does not own.
Three mechanisms have to agree, and each looks healthy on its own while the
combination is broken. The ControlPlane's allowlist
(`spec.korc.serviceRegistrations.allowedNamespaces`) decides which namespaces may
register. The tenant store the ControlPlane provisions into an admitted namespace
carries the credentials back out. The KeystoneService controller projects the
K-ORC children into the ControlPlane's namespace, beside the admin credential
K-ORC authenticates them with, and delivers only the consumer Secret into the
registration's own namespace.

The suite runs three legs against a live Keystone, OpenBao, ESO and cert-manager:

1. **Positive**: an allowlisted namespace registers a catalog entry and an
   account. Its tenant-store trio appears, the seven projected K-ORC children
   reach `Available` in the ControlPlane's namespace, the consumer Secret
   `workflow-credentials` is materialised with its `password` and `clouds.yaml`
   keys, and a Job in that namespace runs `openstack token issue` and
   `openstack catalog show workflow` with it. Authenticating is what separates a
   delivered Secret from a correct one.
2. **Negative**: a `KeystoneService` in a namespace the allowlist never carries
   holds at `NamespaceNotAllowed` on both blocks, so its `Ready` reads
   `False/NotAllReady`, with nothing projected in either namespace and a
   condition message naming the field that would admit it. The check repeats after a settle, so a projection still in
   flight cannot pass as none.
3. **Freeze**: removing the namespace from the allowlist flips its registration
   to `NamespaceNotAllowed` while the consumer Secret, the Keystone user and the
   tenant store all stay. That is decision D9: the allowlist is an admission gate,
   not a revocation tool, and an edit to it can never strand a running service.
   Re-listing the namespace recovers the registration.

Its ControlPlane is its own. The webhook admits one ControlPlane per namespace,
and the canonical `controlplane-keystone` belongs to the full-chain suite, so this
one runs a Keystone-only plane named `cp` in the ephemeral test namespace. A
managed plane outside `openstack` needs two OpenBao prerequisites that
`deploy-infra` seeds only for the identity it was given: the Model B admin
password at `bootstrap/{namespace}/{controlplane}-keystone/admin` and the
database-engine role behind the dynamic DB credential. The suite seeds both with
the same scripts the deploy stack runs (`write-bootstrap-secrets.sh` with
`KORC_CONTROLPLANES` pointing at its own identity, then `setup-database-tenant.sh`).
Everything else the plane needs in its namespace is operator-projected or
cluster-scoped.

The teardown is part of the contract rather than cleanup: deleting the
registration has to reach Keystone through K-ORC, drop the consumer Secret with
its ExternalSecret, and let the plane collect the tenant store once the namespace
holds no registration.

Run it locally against a full ControlPlane stack with
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-controlplane`, which runs it after
the full-chain suite. It is the second chainsaw step of the `e2e-controlplane` CI
job.

### keystone-service

Covers the registration form every built-in service will use: a `KeystoneService`
in the ControlPlane's own namespace, which needs no allowlist entry. Register,
rotate, collide loudly, delete cleanly: each behaviour is unit-tested on a fake
client and again in envtest with K-ORC as schema, and neither of those sees
Keystone, OpenBao, ESO or cert-manager. No other e2e asserts `ServiceCollision`
or `ServiceAccountCollision`.

Five legs run against the live stack:

1. **Round-trip**: `workflow` declares both blocks with
   `controlPlaneRef.namespace` unset and reaches `Ready=True/AllReady`,
   `CatalogReady=CatalogRegistered`, `AccountReady=AccountProvisioned`. Seven
   K-ORC children named `workflow-<hash>-registration-<discriminator>` reach
   `Available` in the plane's namespace, each carrying a controller owner
   reference to the CR, which the suite asserts. The consumer
   Secret `workflow-credentials` is materialised with its `password` and
   `clouds.yaml` keys, the OpenBao leaf under
   `openstack/keystone/{namespace}/workflow/service-accounts/credentials` carries
   the same password, and a Job authenticates with the Secret and shows the
   catalog row. The plane reports
   `ServiceAccountsReady=NoServiceRegistrationsProjected` and
   `RegistrationTenantStoresReady=NoRegistrationNamespaces`.
2. **Latched errors**: the suite injects a terminal
   `Progressing=False/InvalidConfiguration` condition on the registration's
   Project child through the status subresource, because the pinned K-ORC
   retries transport errors itself. With a 409 message, `AccountReady` holds at
   `ServiceAccountsFailed` for 60 s and the Project keeps its condition. With a
   `connection refused` message, the operator clears the condition within
   180 s, the registration returns to `Ready=True` with
   `AccountReady=AccountProvisioned` within 300 s, and
   `status.account.projectID` is unchanged.
3. **Rotation**: a `CredentialRotation` with `reMint: true` rotates the account
   password. Within 300 s the consumer password changes,
   `status.account.passwordGeneration` reaches 2, `lastPasswordRotation` is set,
   `…-password-v2` appears and `…-password-v1` is pruned. The User child's
   `appliedPasswordRef` and its `cobaltcore.c5c3.io/password-generation`
   annotation follow. The verify Job then runs with the old password in hand and
   proves the new one authenticates while the old one is rejected.
4. **Collision** (decision D6): an admin Job seeds a `metering` catalog row and a
   `legacy` user, then `legacy` registers against both with no `adopt` and holds
   at `CatalogReady=False/ServiceCollision` and
   `AccountReady=False/ServiceAccountCollision`, each message naming the field
   that consents to a takeover. The waits gate on those reasons, since
   `ProbingForCollision` also reports `Ready=False` while the imports resolve.
   Nothing is taken over, re-checked after a 30 s settle. Patching
   `catalog.adopt: true` takes the row over at its seeded id, with no
   duplicate; the account half keeps holding; deleting the CR removes the adopted
   row from Keystone while the never-adopted user stays. The suite stops short of
   account adoption: with the pinned K-ORC an adopted user keeps its pre-existing
   password at generation 1, tracked as #920.
5. **Deletion**: deleting `workflow` leaves no K-ORC object under its labels, no
   consumer Secret, ExternalSecret, PushSecret, source Secret or password Secret,
   no live value at the OpenBao leaf, and no service or user row in Keystone. The
   plane stays `Ready=True` with `NoServiceRegistrationsProjected`.

Its ControlPlane is its own: the webhook admits one per namespace, so the suite
runs a Keystone-only plane named `cp` in the ephemeral test namespace and seeds
the same two OpenBao prerequisites the sibling does (`write-bootstrap-secrets.sh`
with `KORC_CONTROLPLANES` pointing at its own identity, then
`setup-database-tenant.sh`). Before the first registration lands it waits for
`InfrastructureReady`, `DBCredentialsReady`, `KeystoneReady`, `KORCReady`,
`AdminCredentialReady`, `ESOTenantStoreReady` and `Ready`. The tenant-store wait
is the one addition over the sibling: the registration's
ExternalSecret and PushSecret ride the plane's own-namespace
`openbao-tenant-store`, so the store is proven up before a delivery that never
starts can be blamed on the registration.

Two Job fixtures do the looking. `workflow-verify` mounts the consumer Secret and
runs `openstack token issue` and `openstack catalog show`, templated over the
registration name, the catalog entry to show, and the endpoint URL that entry
must carry. The rotation leg additionally hands it the pre-rotation password
through a Secret it creates and deletes around the run, so the cleartext never
lands in the Job object.
`admin-osc-seed` and `admin-osc-show` mount the plane's `k-orc-clouds-yaml` and
drive the same CLI as the cloud admin, for the rows only an admin can see or
create. Both print a `SERVICE_ID=` and a `USER_ID=` line the test script parses
out of the Job log, with `ABSENT` for a row that is gone.

Run it locally against a full ControlPlane stack with
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-controlplane`, which runs it third,
after the full-chain and foreign-namespace suites. It is the third chainsaw step
of the `e2e-controlplane` CI job.

## File Layout

```text
tests/e2e/c5c3/
├── admin-password-scoping/
│   ├── chainsaw-test.yaml              Per-CR admin-password projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── db-credential-scoping/
│   ├── chainsaw-test.yaml              Per-CR DB-credential projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── dedicated-backing-services/
│   ├── chainsaw-test.yaml              Per-service dedicated database/cache opt-in
│   └── 00-controlplane-cr.yaml         ControlPlane CR (cp; ephemeral namespace)
├── dedicated-namespaces/
│   ├── chainsaw-test.yaml              Per-service dedicated namespaces + lifecycles
│   ├── 00-controlplane-cr.yaml         ControlPlane CR (cp; @KEYSTONE_NS@/@HORIZON_NS@ tokens)
│   └── 01-keystoneservice-nova.yaml    The parked registration in the Managed Keystone namespace (@KEYSTONE_NS@/@CP_NS@ tokens)
├── deletion-orchestration/
│   ├── chainsaw-test.yaml              ORC-teardown finalizer sequencing
│   └── 00-controlplane-cr.yaml         ControlPlane CR (deletion-orch)
├── external-keystone/
│   ├── chainsaw-test.yaml              External mode vs a plain, operator-free Keystone
│   ├── 00-fixture-keystone.yaml        Plain SQLite Keystone fixture (no operator)
│   ├── 01-fixture-catalog-setup-job.yaml  Non-default identities + duplicate service
│   ├── 02-admin-password-secret.yaml   Fixture admin password for the main CR
│   ├── 02-controlplane-external.yaml   Main External CR
│   ├── 03-controlplane-wrong-password.yaml  Wrong-password negative case
│   ├── 04-controlplane-stalled.yaml    Unreachable-internal-endpoint case
│   ├── 05-controlplane-ambiguous.yaml  Ambiguous-catalog negative case
│   ├── 06-openstack-verify-job.yaml    openstack CLI verify Job
│   ├── 07-keystoneservice-nova.yaml    The brownfield registration (nova; compute + account)
│   └── 08-nova-verify-job.yaml         openstack CLI verify Job on nova-credentials
├── full-controlplane-keystone/
│   ├── chainsaw-test.yaml              Full chain, link by link
│   ├── 00-controlplane-cr.yaml         ControlPlane CR (controlplane-keystone)
│   ├── 01-openstack-verify-job.yaml    openstack CLI verify Job
│   ├── 02-horizon-secret-key-externalsecret.yaml  Per-CP Horizon secret key
│   ├── 03-ovncentral-cr.yaml           Standalone OVNCentral the ControlPlane references
│   ├── 04-ovnchassis-cr.yaml           OVNChassis on the labelled node, which binds the server's port
│   ├── 05-neutronmetadataagent-cr.yaml Metadata agent on that chassis, gated on the generated secret
│   ├── 06-fake-compute.yaml            nova-compute built out of the published compute contract
│   └── 07-hypervisor-operator-verify-job.yaml  Calls as the hypervisor operator's user
├── invalid-cr/
│   ├── chainsaw-test.yaml              ControlPlane admission rejections
│   ├── _generate.py                    Canonical scaffold + generator for the fixtures
│   ├── test_generate.py                Generator unit tests (make verify-invalid-cr-fixtures)
│   └── NN-*.yaml                       One rejected ControlPlane CR per rule
├── invalid-keystoneservice-cr/
│   ├── chainsaw-test.yaml              KeystoneService admission rejections
│   ├── _generate.py                    Canonical scaffold + generator for the fixtures
│   ├── test_generate.py                Generator unit tests (make verify-invalid-cr-fixtures)
│   └── NN-*.yaml                       One rejected KeystoneService CR per rule
├── invalid-sizingprofile-cr/
│   ├── chainsaw-test.yaml              SizingProfile admission rejections
│   ├── _generate.py                    Canonical scaffold + generator for the fixtures
│   ├── test_generate.py                Generator unit tests (make verify-invalid-cr-fixtures)
│   └── NN-*.yaml                       One rejected SizingProfile per rule
├── keystone-service/
│   ├── chainsaw-test.yaml              Own-namespace registration, five legs
│   ├── 00-controlplane-cr.yaml         Keystone-only ControlPlane (cp; no allowlist)
│   ├── 01-keystoneservice-workflow.yaml  The round-trip registration (workflow)
│   ├── 02-keystoneservice-legacy.yaml  The colliding registration (legacy)
│   ├── 03-openstack-verify-job.yaml    openstack CLI verify Job on the consumer Secret
│   ├── 04-openstack-admin-job.yaml     openstack CLI admin Job (seed / show)
│   └── 05-credentialrotation-workflow.yaml  Rotation trigger for workflow
├── keystone-service-foreign-namespace/
│   ├── chainsaw-test.yaml              Cross-namespace registration, three legs
│   ├── 00-controlplane-cr.yaml         Keystone-only ControlPlane (cp; @TENANT_NS@ token)
│   ├── 01-keystoneservice-tenant.yaml  The admitted registration (workflow)
│   ├── 02-keystoneservice-outsider.yaml  The refused registration (outsider)
│   └── 03-openstack-verify-job.yaml    openstack CLI verify Job on the consumer Secret
├── messaging/
│   ├── chainsaw-test.yaml              Shared RabbitMQ bus: provisioned, owned, sized, ready, torn down
│   └── 00-controlplane-cr.yaml         ControlPlane CR (cp; ephemeral namespace)
├── multi-controlplane/
│   ├── chainsaw-test.yaml              Two-tenant isolation contract
│   ├── 00-tenant-a-controlplane.yaml   ControlPlane CR controlplane-a
│   ├── 01-tenant-b-controlplane.yaml   ControlPlane CR controlplane-b
│   └── 02-tenant-a-rotation.yaml       Rotation trigger for tenant-a only
├── secret-store-scoping/
│   └── chainsaw-test.yaml              Per-tenant OpenBao identity + isolation
└── sizing-profile/
    ├── chainsaw-test.yaml              SizingProfile resolution, edit and deletion
    └── 00-controlplane-cr.yaml         ControlPlane CR (cp; __PROFILE__ token)
```

## Related Resources

- [ControlPlane CRD API Reference](../c5c3/controlplane-crd.md) — CRD types, webhooks, validation rules
- [ControlPlane Reconciler](../c5c3/controlplane-reconciler.md) — Sub-reconciler contracts, finalizer, credential re-push
- [CI Workflow](../ci-cd/ci-workflow.md) — The dedicated `e2e-controlplane` job
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — `WITH_CONTROLPLANE` deployment wiring
- `tests/e2e/chainsaw-config.yaml` — Shared Chainsaw configuration
- `hack/ci-check-node-budget.sh` — The node budget gate of Link 6z

### federated-controlplane

`tests/e2e-controlplane-sso/` — the end-user SSO experience the ControlPlane
drives from its Keystone child's identity backends.

A **separate suite and a separate CI job** (`e2e-controlplane-sso`), not an
extension of `full-controlplane-keystone`: the identity-provider and directory
fixtures would otherwise lengthen that chain and couple its credential
assertions to federation, and — decisively — the ControlPlane webhook permits
one ControlPlane per namespace while `openstack-gw` sets
`allowedRoutes.namespaces.from: Same`. The two suites can share neither the
`openstack` namespace nor the Gateway, so each needs its own kind cluster.

It lives **outside `tests/e2e/`** (like `tests/e2e-operator-upgrade/`) because
it keeps declarative `assert` steps rather than the single guarded script step
`full-controlplane-keystone` uses. Chainsaw has no step-level skip, so a
presence guard cannot stop those asserts from running; moving the suite is what
keeps the per-CR `e2e-operator` job and `make e2e` from sweeping it up.

| Step | Behaviour Validated |
| --- | --- |
| 1. `controlplane-ready` | Keycloak, OpenLDAP, and the per-CP Horizon `SECRET_KEY` ExternalSecret come up; the ControlPlane reaches aggregate `Ready` |
| 2. `backends-ready` | Both `KeystoneIdentityBackend` CRs reach `Ready` and the Keystone child reports `IdentityBackendsReady=AllBackendsProjected` |
| 3. `projections` | Attaching the backends is the ONLY action taken, yet the Horizon child now carries the websso choices and multi-domain support, and the Keystone child the trusted origin and the `dev`-tagged sidecar image |
| 4. `rendered-settings` | The rendered `local_settings.py` carries `WEBSSO_ENABLED`, `WEBSSO_USE_HTTP_REFERER = False`, `SECURE_PROXY_SSL_HEADER`, and multi-domain support — but neither `OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN` nor `OPENSTACK_KEYSTONE_DOMAIN_CHOICES`, which would bound the domain field to the domains the operator can enumerate |
| 5. `browser-sso-round-trip` | Waits for the Horizon **and** the Keystone child to observe their final spec and for both Deployments' rollouts to land, then one in-cluster browser, one cookie jar, three flows: (a) the login page offers the SSO choice and a free-text domain field; (b) the websso round trip completes through the gateway against the origin Keystone matches verbatim; (c) an LDAP-domain user logs in through that field |
| 6. `detach` | Deleting both backends clears `spec.websso` and `spec.multiDomain`; `trustedDashboards` survives, since it is derived from `services.horizon`, not from the backends |

**The browser runs in-cluster.** Unlike the gateway quick-start smokes (which
curl from the CI host through the kind `:443` → NodePort bridge), this suite
cannot: mid-flow the browser is redirected to Keycloak's issuer, the in-cluster
`keycloak.openstack.svc.cluster.local` name the host cannot resolve. Exposing
Keycloak through the gateway instead would need a split-horizon DNS rewrite,
since `mod_auth_openidc` must reach the same issuer from inside the cluster.
The browser is therefore the Keystone pod (the image ships `python3`, no
`curl`), dialling the Envoy data-plane ClusterIP with the gateway hostname as
SNI and `Host`, so traffic traverses Envoy exactly as a real browser's would.

The ControlPlane CR pins `services.keystone.federationProxyImage.tag: dev` so
the suite exercises the `mod_auth_openidc` sidecar built by the pipeline, not
the `:latest` already published on `main`.
