---
title: Nova E2E Test Suites
quadrant: operator
---

# Nova E2E Test Suites

Reference documentation for the Nova Chainsaw E2E test suites. These tests
validate the NovaReconciler's end-to-end behavior in a real Kubernetes cluster
with the infrastructure dependencies deployed (MariaDB, Memcached, ESO, OpenBao
and the kind RabbitMQ broker), and, in the suites that boot a server, against a
Keystone, an OVNCentral, a Neutron, a Placement and a Glance the suite brings up
itself.

For the CRD validation E2E tests, see
[Nova CRD](../nova/nova-crd.md#chainsaw-e2e-tests). For the reconciler
architecture and sub-reconciler contracts, see
[Nova Reconciler Architecture](../nova/nova-reconciler.md). For infrastructure
deployment automation, see
[Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md).

## Overview

The suites cover the reconciler lifecycle from a first deployment through
scaling, the three optional HTTP front ends, a cross-release upgrade, the
archive of soft-deleted rows and deletion. Each one creates its own `Nova` CR in
the shared `openstack` namespace. Two sit outside it: `pod-security-restricted`
owns a `nova-pss` namespace labelled with the restricted profile, and
`nova-operator/metrics` installs a second nova-operator Helm release in
`nova-system`.

### Fixture tiers

Eight suites run a Nova with nothing beside it but the broker: `scale`,
`healthcheck`, `httproute`, `network-policy`, `deletion-cleanup`,
`maintenance-endpoint-isolation`, `pod-security-restricted` and
`gateway-quick-start-smoke`. `SecretsReady` reads Secrets, the API and metadata
probes GET the API root, and the scheduler and conductor probes read the broker
socket, so the CR reaches `Ready=True/AllReady` with no Keystone, Placement,
Neutron or Glance in the cluster. The CRD requires `spec.keystoneEndpoint` and
`spec.serviceUser` all the same, so these fixtures set the endpoint to a
convention URL nothing serves and point the service user at the kind-only
`keystone-admin` Secret from `deploy/kind/infrastructure`. Nothing validates
that password here.

`remote-compute-contract` runs lighter still, with no broker either. It asserts
the two compute contracts the ComputeConfig step publishes, and that step runs
before the Database step, so no step it waits on dials a backend. Every address
in its CR is a placeholder, and its Nova never reaches `Ready`.

The four suites that boot a server carry the whole stack a boot touches:
`basic-deployment`, `basic-deployment-2026-1`, `db-archive` and `console-proxy`
each bring their own Keystone and a catalog Job that writes the compute,
placement, image and network rows, an OVNCentral and the Neutron that programs
it, a Placement the scheduler claims from, a Glance with an S3 backend and a
seeded image, the Nova, and a fake-driver compute. `release-upgrade` sits
between the tiers with a Keystone and a Placement: the expand phase runs
`nova-status upgrade check`, and that check reads Placement as an authenticated
client.

Each of those five Keystones has a Memcached CR of its own, applied from the
same `00-keystone-cr.yaml` and named `<keystone>-cache`. Keystone caches the
lookup of a user by name under a key that carries no instance identity, so two
Keystones on `openstack-memcached` read each other's entry: the second resolves
`admin` to the first one's user id and answers 401, and nova-conductor and
nova-scheduler crash on that while they build their Placement client. The other
services of a suite stay on `openstack-memcached`.

The servers are created with `--nic none`, so no port is ever bound, and the
Neutron is there all the same. The compute API resolves `[neutron]` on every
request that touches ports, and the metadata API verifies a proxied request
against the shared secret the network service's agents sign with, so a
deployment without a network service in the catalog is not the deployment the
suites are about.

Fixtures are copies, not a shared tree. A full-stack suite names no file outside
its own directory except `../../cinder/broker-vhost.sh` and
`../discover-hosts.sh`, so a change to one belongs in the others.

### One RabbitMQ vhost per suite

`tests/e2e/cinder/broker-vhost.sh`, reused from the cinder tree, gives each
suite a private queue namespace on the kind-only `shared-rabbitmq` broker. The
`create` form reads the broker's default-user Secret, runs `rabbitmqctl
add_vhost` and `rabbitmqctl set_permissions` inside the broker pod, and applies
a Secret whose `transport_url` key carries the vhost path. Every suite names its
CR as the vhost and `<cr>-messaging` as the Secret, which the `Nova` CR then
references through `spec.messaging.secretRef`.

The vhost is what keeps the suites apart: nova's RPC topics are named after the
service rather than after the CR, so two suites on one vhost would share the
scheduler and conductor queues and a compute could answer the other suite's API.
Managed messaging (`spec.messaging.clusterRef`) does not work here, because the
URL it renders always names the root vhost. The first step of a suite calls the
`create` form, and that step's `cleanup` block calls the `delete` form for the
same vhost at the end of the test, guarded so a cleanup failure cannot redden a
run whose assertions all passed.

### Host discovery

A compute service registers itself in the cell database when it starts, and the
scheduler places servers only on a host that also has a host mapping in the API
database. `tests/e2e/nova/discover-hosts.sh` runs `nova-manage cell_v2
discover_hosts --verbose` inside the conductor pod, the one workload that holds
both database connections, and then reads `cell_v2 list_hosts` for the host
(`fake-1`). Both repeat 5 seconds apart for up to 150 seconds, since a discovery
that runs before the compute has registered maps nothing. `db-archive` and
`console-proxy` call it, and so do the `nova-broker-outage` and
`nova-placement-outage` chaos suites.

The two `basic-deployment` suites do not. They wait out the scheduler's own
periodic instead, which is what says the periodic runs:
`discover_hosts_in_cells_interval` renders as 300 seconds, the scan runs at
scheduler start and then once an interval, and the compute registers after that
first pass. The interval is never overridden, because the key is Reported in
`operators/nova/api/v1alpha1/config_ownership.go` and a `spec.extraConfig` entry
for it would flip `ExtraConfigHealthy`.

### The CI leg

The nova `e2e-operator` leg deploys five sibling operators through
`hack/ci-deploy-operator.sh` before the nova-operator (keystone, placement,
glance, ovn and neutron), each into its own `<op>-system` namespace, and passes
`WITH_MESSAGING: true` to the infrastructure bring-up, and
`WITH_OVN_KERNEL_MODULES: true` for the single-node chassis `compute-node-pool`
runs. Chainsaw runs with
`--parallel 2` rather than the shared config's four, under a 150-minute wall
instead of the 68 the other legs take: a full-stack Nova suite is a Keystone, an
OVNCentral, a Neutron, a Placement, a Glance and the five Nova workloads. Beside
the operator and service images the leg loads `tempest:2025.2`, which is where
the catalog, seed and verify Jobs get their `openstack` client, and
`nova-compute` at both nova releases, the image a NovaCompute pool runs.

The leg runs as two shards, each on a kind cluster of its own and under its own
150-minute wall. Shard 2 runs `compute-node-pool`, `invalid-novacompute-cr`,
`basic-deployment-2026-1`, `release-upgrade`, `healthcheck`, `deletion-cleanup`
and `pod-security-restricted`. Shard 1 runs every other suite, so a new suite
runs there until the `Run E2E tests` step in `.github/workflows/ci.yaml` names
it for shard 2. See [CI Workflow](../ci-cd/ci-workflow.md#e2e-operator).

## Prerequisites

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | `WITH_MESSAGING=true make deploy-infra` (see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Nova operator | nova-operator chart installed with the CRD registered and the webhooks active |
| Sibling operators | keystone, placement, glance, ovn and neutron operators, for the suites that boot a server |
| ESO ExternalSecrets | `nova-api-db` and `nova-db` synced in the `openstack` namespace |
| Service-user Secret | the kind-only `keystone-admin` Secret in `openstack`, read by every `spec.serviceUser` |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` |
| Message broker | `shared-rabbitmq` RabbitmqCluster in `openstack` (`WITH_MESSAGING=true`) |
| Gateway | `GatewayClass/envoy` and `Gateway/openstack-gw` with the `https-nova`, `https-nova-metadata` and `https-nova-console` listeners, for the two suites that curl them |
| Service images | `ghcr.io/c5c3/nova:2025.2` for every suite, `ghcr.io/c5c3/nova:2026.1` for `basic-deployment-2026-1` and the target half of `release-upgrade`, and `ghcr.io/c5c3/tempest:2025.2` for the catalog, seed and verify Jobs |
| Chainsaw | the `CHAINSAW_VERSION` pinned in `hack/install-test-deps.sh` |

## Running the Tests

```bash
# Run the Nova suites the way the e2e-operator CI leg does
chainsaw test --config tests/e2e/chainsaw-config.yaml --parallel 2 \
  tests/e2e/nova/ tests/e2e/nova-operator/

# Run a single suite
chainsaw test --config tests/e2e/chainsaw-config.yaml \
  --test-dir tests/e2e/nova/basic-deployment
```

## Chainsaw Configuration

All tests use the shared configuration at `tests/e2e/chainsaw-config.yaml`:

| Setting | Value | Purpose |
| --- | --- | --- |
| `timeouts.apply` | 30s | Resource application timeout |
| `timeouts.assert` | 120s | Default assertion timeout (every Nova suite overrides it) |
| `timeouts.cleanup` | 3m | Post-test resource cleanup |
| `timeouts.delete` | 30s | Resource deletion timeout |
| `timeouts.error` | 30s | Error assertion timeout |
| `timeouts.exec` | 30s | Script execution timeout |
| `execution.parallel` | 4 | Maximum concurrent test suites (the CI leg narrows this to 2) |
| `execution.failFast` | true | Stop on first failure |
| `report.format` | JUNIT-TEST | JUnit XML output for CI |
| `report.path` | `_output/reports` | Report directory |

Every suite raises `timeouts.assert` above the shared 120 s, because a
two-schema migration, the cell mapping and the cold start of five workloads all
precede the first assertion. Every suite that brings a Nova to Ready settles on
10 minutes, because the first CI run measured the bring-up at about 8: the eight
MariaDB CRs take 3 minutes at the operator's 30 s requeue and the six
nova-manage invocations of the db-sync about 3 more. `release-upgrade` keeps
the same 10 for the three phase Jobs and the five-Deployment rollout behind the
patch, `gateway-quick-start-smoke` takes 15, and the chart-level `metrics`
suite, which brings no Nova, keeps 5. The script envelope is raised with it:
`timeouts.exec` is 12 minutes in the two `basic-deployment` suites and
`db-archive`, 15 in `gateway-quick-start-smoke`, and 25 in `console-proxy`,
whose bring-up and boot run in one script. `deletion-cleanup` also raises
`timeouts.error` to 3 minutes, because a Nova brings eight MariaDB CRs rather
than three.

Four suites opt out of the defaults for a reason of their own. `scale`,
`gateway-quick-start-smoke` and `compute-node-pool` set `concurrent: false`, the
first because its peak of ten pods at the shared 100m request holds 1000m on the
single kind node, the second because it and `console-proxy` claim the same
console hostname on the one Gateway, and the third because its chassis and
nova-compute pods own host paths on the one node (see
[tests/e2e/README.md](https://github.com/C5C3/cobaltcore/blob/main/tests/e2e/README.md)). `pod-security-restricted` sets `spec.namespace: ""` to opt out of
Chainsaw's per-test namespace, which carries no PodSecurity labels, and applies
a labelled namespace of its own.

## Test Suite Inventory

| Suite | CR Name | Reconciler Behavior Validated |
| --- | --- | --- |
| [basic-deployment](#basic-deployment) | `nova-basic` | Happy path on 2025.2: fifteen sub-conditions, the five Deployments and their owned children, the rendered `nova.conf` and the compute contract, a server booted, resized and deleted on a fake-driver compute |
| [basic-deployment-2026-1](#basic-deployment-2026-1) | `nova-basic-2026-1` | The same assertions against the 2026.1 image, with the API container image pinned, so a difference between the two releases fails here |
| [scale](#scale) | `nova-scale` | `spec.api.deployment.replicas` 3 → 5 → 1 with the PodDisruptionBudget policy flipping, the other four Deployments left at the counts their own spec fields give them |
| [healthcheck](#healthcheck) | `nova-health` | `NovaAPIReady=True/APIHealthy` and the cluster-local `status.endpoint` |
| [httproute](#httproute) | `nova-route` | The three gateway blocks: routes for the API on 8774, the metadata API on 8775 and the console proxy on 6080, `HTTPRouteNotAccepted` then `HTTPRouteAccepted`, deleted with the spec blocks |
| [network-policy](#network-policy) | `nova-netpol` | Two rendered NetworkPolicies: one covering every pod of the CR with the auto-derived egress order, one carrying the console proxy's display range, updated and deleted |
| [deletion-cleanup](#deletion-cleanup) | `nova-cleanup` | Finalizer cleanup of every owned child and of the eight MariaDB CRs, the two cell0 objects included |
| [pod-security-restricted](#pod-security-restricted) | `nova-pss` | Every Pod the reconciler projects admits under `pod-security.kubernetes.io/enforce=restricted`, on brownfield database wiring and with zero `FailedCreate` violations |
| [gateway-quick-start-smoke](#gateway-quick-start-smoke) | `nova-smoke` | The three external URLs the quick start names answer 200 through `openstack-gw` |
| [maintenance-endpoint-isolation](#maintenance-endpoint-isolation) | `nova-isolation` | A live db-archive pod is never an address of the API, metadata or console Service, and none of the three is left without backends |
| [db-archive](#db-archive) | `nova-archive` | The archive CronJob fires on a minute schedule and moves the deleted server into the shadow tables, with no `DBArchiveJobFailed` event on the CR |
| [release-upgrade](#release-upgrade) | `nova-upgrade` | Cross-release upgrade 2025.2 to 2026.1: phase progression, the three phase Jobs, the five Deployments, the cell mappings and the API on the new release |
| [compute-node-pool](#compute-node-pool) | `nova-pool`, pools `pool-a` and `pool-b` | A NovaCompute on the fake driver: Ready, the node Active with its service up, the wait-for-chassis gate, both aggregates marked, a conflicting second pool, the drain of a node with a server on it, the release, and the teardown of the last pool |
| [console-proxy](#console-proxy) | `nova-vnc` | The console URL the API publishes carries the gateway hostname, the token handshake through it reaches the instance console, and an invalid token is turned down |
| [remote-compute-contract](#remote-compute-contract) | `nova-rc` | The remote compute contract `spec.remoteCompute` publishes: both status refs, the six keys, the external transport URL, a fragment addressed at the public Keystone URL and the public catalog rows, the in-cluster contract unchanged, and the Secret gone once the block is removed |
| [invalid-cr](#invalid-cr) | (rejected at admission) | `Nova` rejection corpus: the release pattern, the image and database and cache and messaging union rules, the messaging TLS rule, the archive and scheduler bounds, the two cross-database rules, the cell0 name rules, both `extraConfig` guards, the two name rules, the URL fields, the console gateway path and the two `spec.remoteCompute` rules. See [Nova CRD](../nova/nova-crd.md#chainsaw-e2e-tests) |
| [invalid-novacompute-cr](#invalid-novacompute-cr) | (rejected at admission) | `NovaCompute` rejection corpus: the novaRef, selector, name and target rules, the offboarding toleration by key and as a wildcard, the cpuModels rule both ways, the two libvirt enums, a rejected extraConfig key, maxUnavailable under OnDelete and the image pin. See [NovaCompute CRD](../nova/novacompute-crd.md#defaulting-and-validation) |
| [metrics](#metrics) | — (operator-level) | nova-operator chart renders and removes the ServiceMonitor |

---

## Test Suite Details

### basic-deployment

**File:** `tests/e2e/nova/basic-deployment/chainsaw-test.yaml`

**Purpose:** The full reconciliation cycle of a Nova on the 2025.2 release
against real pods, a real broker vhost and a fake-driver compute. All fifteen
sub-conditions reach True with their reasons, status publishes what a client and
a compute cluster read off the CR, the five workloads carry the commands and
probes the image supports, and a server boots, resizes and is deleted through
the `openstack` client.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then bring up Keystone | `script` (2m) + `apply` + `assert` | `broker-vhost.sh create nova-basic nova-basic-messaging openstack`, then `00-keystone-cr.yaml` with `keystone-nova-basic` asserted `Ready=True/AllReady`. The step cleanup deletes the vhost at the end of the test |
| 2 | Register the four services in the catalog | `script` + `assert` | Deletes any leftover Job, applies `01-catalog-setup-job.yaml` and waits 5m for completion; `succeeded: 1` |
| 3 | Bring up the four services the boot path depends on | `apply` + `assert` | `02-messaging-secret.yaml`, then `nova-basic-ovn`, `neutron-nova-basic`, `placement-nova-basic`, `glance-nova-basic` and `glance-nova-basic-s3`, each asserted `Ready=True/AllReady` |
| 4 | Seed the image the server boots from | `script` + `assert` | `08-image-seed-job.yaml`, waited 5m, `succeeded: 1` |
| 5 | Assert the conditions, the children and the config | `apply` + `assert` + `script` | The fifteen sub-conditions with their reasons (`SecretsAvailable`, `ComputeConfigPublished`, `DatabaseSynced`, `ConductorReady`, `SchedulerReady`, `MetadataReady`, `ConsoleProxyReady`, `DeploymentReady`, `DBArchiveScheduled`, `APIHealthy`, `HPANotRequired`, `NetworkPolicyNotRequired`, three times `HTTPRouteNotRequired`, `NoOwnedKeysOverridden`) and `Ready=True/AllReady`; `status.endpoint`, `installedRelease: "2025.2"`, `computeConfigSecretRef`, cell0 under the all-zero uuid and one cell1. The API Deployment runs `--module nova.wsgi.osapi_compute:application` with no `--pyargv`, reads `OS_NOVA_CONFIG_DIR` and `OS_NOVA_CONFIG_FILES`, sources both database URLs and the transport URL from Secrets, and has `availableReplicas: 2`; the metadata API adds `metadata.conf` and the shared secret; the scheduler and the conductor take readiness off `/var/lib/openstack/bin/nova-amqp-ready`, carry no `livenessProbe` and terminate on a 200-second grace period; the console proxy reads `/vnc_lite.html` on 6080. Three Services (8774, 8775, 6080), a PDB with `minAvailable: 1` excluding Job pods, the `@daily` archive CronJob on `archive_deleted_rows --all-cells` without `--purge` or `--until-complete`, the db-sync Job, and the compute-config Secret with its five keys and no `ca.crt`. Two scripts read the rendered documents (see below) |
| 6 | Start the fake-driver compute | `apply` + `assert` | `11-fake-compute.yaml`, applied only now because it mounts the compute-contract Secret; `nova-basic-fake-compute` reaches `availableReplicas: 1` |
| 7 | Wait for the scheduler to map the compute into cell1 | `script` (10m) | Polls `nova-manage cell_v2 list_hosts` in the conductor for `fake-1`, 84 times 5 s apart, then greps `Discovered 1 new hosts` out of the scheduler log |
| 8 | Boot, resize and delete a server | `script` + `assert` | `12-verify-job.yaml`, waited 10m; the log is read once and `NOVA-VERIFY-OK` grepped out of it; `succeeded: 1` |

The two config scripts in step 5 resolve the content-hashed ConfigMap through
the API Deployment's `config` volume and match
`discover_hosts_in_cells_interval = 300`, `driver = noop`,
`local_metadata_per_cell = false`, the suite's one `extraConfig` override
`allow_resize_to_same_host = true`, the `novncproxy_base_url` pointing at the
proxy Service and `valid_interfaces = internal` inside `[placement]`, and they
fail on a `password` line or the metadata shared secret. The second reads
`nova-compute.conf` out of the compute-config Secret: `[placement]`,
`[neutron]`, `[glance]`, `[service_user]` and `[vnc]` are present, `[database]`
and `[api_database]` are not, and `cell_name` is `cell1`.

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-messaging-secret.yaml`, `03-ovncentral-cr.yaml`, `04-neutron-cr.yaml`,
`05-placement-cr.yaml`, `06-glance-cr.yaml`, `07-glancebackend-cr.yaml`,
`08-image-seed-job.yaml`, `09-metadata-secret.yaml`, `10-nova-cr.yaml`,
`11-fake-compute.yaml`, `12-verify-job.yaml`

---

### basic-deployment-2026-1

**File:** `tests/e2e/nova/basic-deployment-2026-1/chainsaw-test.yaml`

**Purpose:** The per-release twin of `basic-deployment`. Nothing the config step
renders reads `spec.openStackRelease`, so the suite asserts the same conditions,
the same workload shapes and the same `nova.conf` against the 2026.1 image, and
a difference between the two is the failure it exists to catch.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then bring up Keystone | `script` (2m) + `apply` + `assert` | `broker-vhost.sh create nova-basic-2026-1 nova-basic-2026-1-messaging openstack` and `keystone-nova-basic-2026-1` Ready |
| 2 | Register the four services in the catalog | `script` + `assert` | `01-catalog-setup-job.yaml`, `succeeded: 1` |
| 3 | Bring up the four services the boot path depends on | `apply` + `assert` | `nova-basic-2026-1-ovn`, `neutron-nova-basic-2026-1`, `placement-nova-basic-2026-1`, `glance-nova-basic-2026-1` and `glance-nova-basic-2026-1-s3`, each Ready |
| 4 | Seed the image the server boots from | `script` + `assert` | `08-image-seed-job.yaml`, `succeeded: 1` |
| 5 | Assert the conditions, the children and the config | `apply` + `assert` (10m) + `script` | The same fifteen sub-conditions and children as `basic-deployment`, with `installedRelease: "2026.1"` and the API container pinned to `ghcr.io/c5c3/nova:2026.1`, so a release bump that forgot the tag shows up here rather than in a passing 2025.2 run |
| 6 | Start the fake-driver compute | `apply` + `assert` | `nova-basic-2026-1-fake-compute` at one available replica |
| 7 | Wait for the scheduler to map the compute into cell1 | `script` (10m) | The same 300-second periodic, which nova 33.0.0 also runs with `run_immediately=True` |
| 8 | Boot, resize and delete a server | `script` + `assert` | `12-verify-job.yaml` to `NOVA-VERIFY-OK` |

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-messaging-secret.yaml`, `03-ovncentral-cr.yaml`, `04-neutron-cr.yaml`,
`05-placement-cr.yaml`, `06-glance-cr.yaml`, `07-glancebackend-cr.yaml`,
`08-image-seed-job.yaml`, `09-metadata-secret.yaml`, `10-nova-cr.yaml`,
`11-fake-compute.yaml`, `12-verify-job.yaml`

---

### scale

**File:** `tests/e2e/nova/scale/chainsaw-test.yaml`

**Purpose:** Patching `spec.api.deployment.replicas` propagates to the API
Deployment and its PodDisruptionBudget. The scheduler runs two replicas and the
conductor, the metadata API and the console proxy one each, all sized by spec
fields of their own, so a builder that read the API replica count for every
workload would scale them along and only these assertions would catch it.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CR | `script` (2m) + `apply` | `broker-vhost.sh create nova-scale nova-scale-messaging openstack`, then the metadata Secret and `nova-scale` at three API replicas |
| 2 | Assert Ready and the initial replica counts | `assert` (10m) | `Ready=True/AllReady`, the API Deployment at `replicas: 3` with three ready and available, PDB `minAvailable: 1`, and the scheduler (2), conductor, metadata API and console proxy at their own counts |
| 3 | Scale up to 5 and assert the rollout completed | `patch` + `assert` | `availableReplicas: 5` and `updatedReplicas: 5`, so new pods started rather than the spec alone changing, and `Ready=True/AllReady` |
| 4 | Scale down to 1 and assert the PDB flips | `patch` + `assert` | One replica, PDB `maxUnavailable: 1`, the other four Deployments unchanged, `Ready=True/AllReady` |

**Fixtures:** `00-metadata-secret.yaml`, `01-nova-cr.yaml`,
`02-patch-scale-up.yaml`, `03-patch-scale-to-one.yaml`

---

### healthcheck

**File:** `tests/e2e/nova/healthcheck/chainsaw-test.yaml`

**Purpose:** The operator's HTTP probe of the Nova API drives `NovaAPIReady` and
`status.endpoint`. The probe GETs the API root, the only route the compute API
answers without a token and without touching a database or the bus; nova ships
no `/healthcheck` route of its own, which is what separates this probe from the
sibling services.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CR | `script` (2m) + `apply` | `broker-vhost.sh create nova-health nova-health-messaging openstack`, then the metadata Secret and `nova-health` at one API replica |
| 2 | Assert NovaAPIReady and the endpoint | `assert` (10m) | `status.endpoint: http://nova-health.openstack.svc.cluster.local:8774`, `DeploymentReady=True`, `NovaAPIReady=True/APIHealthy` and `Ready=True/AllReady` |

**Fixtures:** `00-metadata-secret.yaml`, `01-nova-cr.yaml`

---

### httproute

**File:** `tests/e2e/nova/httproute/chainsaw-test.yaml`

**Purpose:** Nova publishes three front ends a browser or a peer service reaches
over HTTP, each with a gateway block and a readiness condition of its own, so
the suite drives all three through one lifecycle. It covers the operator's
HTTPRoute contract alone: the step that accepts the routes patches their parent
status rather than running a Gateway controller, so nothing here proves traffic
arrives.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Install the stub CRD, take the vhost, apply the CR | `apply` + `script` (2m) + `apply` | `00-httproute-crd.yaml` first, so the operator can create HTTPRoutes on a cluster with no Gateway controller, then the vhost, the metadata Secret and `nova-route` |
| 2 | Assert the three routes and the unaccepted conditions | `assert` (10m) | `nova-route` on `nova-route.example.test` to backend `nova-route:8774`, `nova-route-metadata` to `nova-route-metadata:8775`, `nova-route-console` to `nova-route-novncproxy:6080`, all three on parent `not-a-real-gateway` with a `PathPrefix` on `/`; `HTTPRouteReady`, `MetadataHTTPRouteReady` and `ConsoleHTTPRouteReady` all `False/HTTPRouteNotAccepted` |
| 3 | Accept all three routes and assert the conditions flip | `script` + `assert` | A status patch publishes `Accepted=True` on each parent; the three conditions read `True/HTTPRouteAccepted` and `status.endpoint` becomes `https://nova-route.example.test/` |
| 4 | Unset the three gateway blocks and assert the deletions | `patch` + `assert` + `script` | The three conditions read `True/HTTPRouteNotRequired` and a script fails if any of the three routes is still there |

**Fixtures:** `00-httproute-crd.yaml`, `01-metadata-secret.yaml`,
`02-nova-cr.yaml`, `03-patch-remove-gateways.yaml`

---

### network-policy

**File:** `tests/e2e/nova/network-policy/chainsaw-test.yaml`

**Purpose:** The NetworkPolicy sub-reconciler renders two policies. The main one
carries no component key in its selector, so it covers every pod of the CR; the
second belongs to the console proxy alone, which dials the VNC server of
whichever hypervisor an instance runs on. The default CI CNI (kindnet) does not
enforce NetworkPolicy, so the suite validates the rendered objects and their
lifecycle rather than traffic.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CR | `script` (2m) + `apply` | `broker-vhost.sh create nova-netpol nova-netpol-messaging openstack`, then the metadata Secret and `nova-netpol` |
| 2 | Assert both policies carry the expected rules | `assert` (10m) | `NetworkPolicyReady=True/NetworkPolicyReady`; the main policy with ingress on 8774, 8775 and 6080 and the auto-derived egress order DNS, api-database 3306, database 3306, cache 11211, Keystone 5000, the siblings 8778/9292/9696 and messaging 5672, plus a check that its selector carries no `app.kubernetes.io/component` key; the console policy egress on TCP 5900 through 65535 |
| 3 | Add a second ingress source and assert it lands | `patch` + `assert` | The operator appends one peer per declared source plus one for its own Namespace, so `length(spec.ingress[0].from) >= 3` is what separates the patched policy from the unchanged one |
| 4 | Disable networkPolicy and assert both are deleted | `patch` + `assert` | `NetworkPolicyReady=True/NetworkPolicyNotRequired` and `error` assertions on both policies |

**Fixtures:** `00-metadata-secret.yaml`, `01-nova-cr.yaml`,
`02-patch-update-ingress.yaml`, `03-patch-disable-networkpolicy.yaml`

---

### deletion-cleanup

**File:** `tests/e2e/nova/deletion-cleanup/chainsaw-test.yaml`

**Purpose:** The finalizer-driven cleanup on Nova CR deletion. Every owned child
is asserted present before the delete, because an absence assertion on a
resource that never existed passes for the wrong reason. The MariaDB side is
eight objects: a Database, User and Grant for the `nova_cleanup_api` schema
under the instance name `nova-cleanup-api`, the same three for `nova_cleanup`
under `nova-cleanup`, and a Database and Grant for cell0, which travels as an
additional schema on the cell block's user and so has no User of its own.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CR | `script` (2m) + `apply` | `broker-vhost.sh create nova-cleanup nova-cleanup-messaging openstack`, then the metadata Secret and `nova-cleanup` |
| 2 | Wait for Ready and assert the cleanup finalizer | `assert` (10m) | `Ready=True/AllReady` and `nova.openstack.c5c3.io/finalizer` among the finalizers, read through a JMESPath fallback so the first poll retries instead of raising a non-retryable type error |
| 3 | Assert every owned child exists before the deletion | `assert` + `script` | Five Deployments, three Services, the PDB, the four derived Secrets (two db-connection, the transport URL and the compute config), the db-sync Job, the archive CronJob, the eight MariaDB objects, and the content-hashed config ConfigMap matched by prefix |
| 4 | Delete the Nova CR | `delete` (2m) | A bounded delete, so a finalizer that blocks removal fails the suite |
| 5 | Assert the owned children and the MariaDB CRs are gone | `error` (3m) | The same objects as step 3, each asserted absent |
| 6 | Assert the content-hashed ConfigMap is gone | `script` (4m) | Polls for up to 3 minutes, since the garbage collector reaps owned objects a moment after the CR leaves etcd |

**Fixtures:** `00-metadata-secret.yaml`, `01-nova-cr.yaml`

---

### pod-security-restricted

**File:** `tests/e2e/nova/pod-security-restricted/chainsaw-test.yaml`

**Purpose:** A Nova reconciles to Ready inside a namespace labelled
`pod-security.kubernetes.io/enforce=restricted`, with every workload kind the
operator projects in the subject: the five Deployments, the db-sync Job and the
archive CronJob. The CR runs in brownfield mode, because the shared provisioning
flow resolves `clusterRef` in the CR's own namespace and a CR in `nova-pss`
cannot reach `openstack/openstack-db` that way.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the namespace and the schemas, seed OpenBao, take a vhost | `apply` + `script` (5m) + `script` (2m) + `assert` | The labelled namespace and the three pre-created schemas with their two users; two namespace-scoped OpenBao paths copied from the shared standalone values, then `setup-eso-tenant.sh nova-pss`; the vhost and the two Secrets the CR reads that no ExternalSecret provides; the enforce labels and all eight MariaDB objects asserted Ready |
| 2 | Assert both credential Secrets sync, then apply the CR | `assert` + `apply` | The `nova-api-db` and `nova-db` ExternalSecrets Ready before `nova-pss` is applied, so a broken ESO sync fails here rather than as a `SecretsReady` symptom |
| 3 | Assert Ready and every workload under the restricted profile | `assert` (10m) + `error` | `Ready=True/AllReady` and the five Deployments at one available replica each; `error` assertions that no Database, User or Grant exists in the test namespace, whatever its name |
| 4 | Run the archive CronJob by hand under the profile | `script` (3m) + `assert` | `kubectl create job --from=cronjob/nova-pss-db-archive` copies the jobTemplate the operator rendered, and the Job reaches `succeeded: 1` |
| 5 | Assert zero PSS-violation FailedCreate events | `script` | A zero-match scan for `violates PodSecurity "restricted:latest"` on `FailedCreate` events in the test namespace |

**Fixtures:** `00-namespace.yaml`, `00-brownfield-db-setup.yaml`,
`01-nova-cr.yaml`

---

### gateway-quick-start-smoke

**File:** `tests/e2e/nova/gateway-quick-start-smoke/chainsaw-test.yaml`

**Purpose:** The three external URLs the quick start tells a user to open work:
`https://nova.127-0-0-1.nip.io/` answers 200 with a `versions` document,
`https://nova-metadata.127-0-0-1.nip.io/` answers 200 with a non-empty body, and
`https://nova-console.127-0-0-1.nip.io/vnc_lite.html` answers 200 with the noVNC
page. The chain runs from the kind host on `:443` through the node port to the
Envoy Gateway proxy, one of the three HTTPS listeners on
`openstack/openstack-gw`, the operator-managed HTTPRoute, the Service and the
pod.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply namespace | `apply` | `00-namespace.yaml` alone. The CR is not applied here: its CRD is registered by the nova-operator, which is suspended in some overlays, so the applies live behind the step-2 presence guard |
| 2 | Smoke-check the three Nova front ends | `script` (15m) | Exits 0 with a `SKIP:` line when `GatewayClass/envoy`, `Gateway/openstack-gw`, the Nova CRD or one of the three listeners is absent. Otherwise it waits for the Gateway to be `Programmed`, takes the vhost, applies the metadata Secret and `nova-smoke`, waits for `DatabaseReady`, then for `HTTPRouteReady`, `MetadataHTTPRouteReady`, `ConsoleHTTPRouteReady` and `NovaAPIReady`, and curls each URL up to 15 times 2 s apart. A `finally` block deletes the CR, the two Secrets and the vhost |

**Fixtures:** `00-namespace.yaml`, `01-metadata-secret.yaml`,
`02-smoke-nova-cr.yaml`

**Design notes:**

- curl runs from the Chainsaw host rather than from an in-cluster pod. The
  nip.io mapping to 127.0.0.1 is an external DNS concern, and
  `hack/kind-config.yaml` already bridges host `127.0.0.1:443` to the proxy
  NodePort.
- The API root is accepted at 200 only. Nova answers `/` with a plain 200 and a
  document whose top-level key is `versions`, where cinder's api-paste routes
  `/` into an `apiversions` application answering 300.
- The suite applies and deletes everything from inside the guarded script, so
  Chainsaw's own cleanup never sees those objects. Without the `finally` block
  each run would leak five Deployments, three HTTPRoutes holding the nip.io
  hostnames, and the eight MariaDB CRs.

---

### maintenance-endpoint-isolation

**File:** `tests/e2e/nova/maintenance-endpoint-isolation/chainsaw-test.yaml`

**Purpose:** Nova is the operator with three HTTP front ends under one instance
label, so each Service narrows its selector by its own component. The archive
pod serves no HTTP and carries no readiness probe, so the endpoints controller
would publish its address the moment it is scheduled, and the component key in
the three selectors is the only thing keeping it out.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CR | `script` (2m) + `apply` | `broker-vhost.sh create nova-isolation nova-isolation-messaging openstack`, then the metadata Secret and `nova-isolation` |
| 2 | Wait for Ready with both API pods up | `assert` (10m) | `Ready=True/AllReady` and the API Deployment at `availableReplicas: 2` |
| 3 | Sample the three Services across the archive run | `script` (5m) | Starts an archive Job from the CronJob without waiting for it, then samples for 90 s: the API Service never carries more addresses than the Deployment's live replica count, the metadata and console Services never more than one, the API Service is seen with its full count at least once, and a live archive pod is observed and its IP checked against all three address sets each tick. Sentinel `ISOLATION-OK` |

**Fixtures:** `00-metadata-secret.yaml`, `01-nova-cr.yaml`

---

### db-archive

**File:** `tests/e2e/nova/db-archive/chainsaw-test.yaml`

**Purpose:** The CronJob the operator projects moves rows rather than only
existing. Nova only ever soft-deletes, so an archive run started after a server
was deleted has to leave the instance in `shadow_instances` with no soft-deleted
row behind in the live table.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then bring up Keystone | `script` (2m) + `apply` + `assert` | `broker-vhost.sh create nova-archive nova-archive-messaging openstack` and `keystone-nova-archive` Ready |
| 2 | Register the four services in the catalog | `script` + `assert` | `01-catalog-setup-job.yaml`, `succeeded: 1` |
| 3 | Bring up the four services the boot path depends on | `apply` + `assert` | The messaging Secret, `nova-archive-ovn`, `neutron-nova-archive`, `placement-nova-archive`, `glance-nova-archive` and `glance-nova-archive-s3`, each Ready |
| 4 | Seed the image the server boots from | `script` + `assert` | `08-image-seed-job.yaml`, `succeeded: 1` |
| 5 | Assert the conditions and the archive CronJob | `apply` + `assert` (10m) | The fifteen sub-conditions, `installedRelease`, the compute-config reference and both cells; the CronJob on the suite's own `* * * * *` schedule with `concurrencyPolicy: Forbid`, `archive_deleted_rows --all-cells` without `--purge` or `--until-complete`, `MAX_ROWS` 1000, `SLEEP` 1 and no `RETENTION_DAYS`, so every soft-deleted row is eligible |
| 6 | Start the fake-driver compute | `apply` + `assert` | `nova-archive-fake-compute` at one available replica |
| 7 | Map the compute into cell1 | `script` (3m) | `../discover-hosts.sh nova-archive` |
| 8 | Boot and delete a server | `script` + `assert` | `12-verify-job.yaml` to `NOVA-VERIFY-OK`; the delete leaves the rows the next step waits on |
| 9 | Wait for a run that saw the delete, then read the tables | `script` (5m) | Stamps the time, polls up to 3 minutes for an archive Job created after it that succeeded, then reads `shadow_instances` and the soft-deleted rows left in `instances` with the `mariadb` client in `openstack-db-0`: at least one shadow row and zero left behind |
| 10 | The operator reports the schedule and no failed run | `assert` + `script` (2m) | `DBArchiveReady=True/DBArchiveScheduled` and no `DBArchiveJobFailed` event on the CR, which the condition alone would not catch after a failed run followed by a successful one |

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-messaging-secret.yaml`, `03-ovncentral-cr.yaml`, `04-neutron-cr.yaml`,
`05-placement-cr.yaml`, `06-glance-cr.yaml`, `07-glancebackend-cr.yaml`,
`08-image-seed-job.yaml`, `09-metadata-secret.yaml`, `10-nova-cr.yaml`,
`11-fake-compute.yaml`, `12-verify-job.yaml`

---

### release-upgrade

**File:** `tests/e2e/nova/release-upgrade/chainsaw-test.yaml`

**Purpose:** The one Nova suite that enters the four-phase upgrade machine of
`operators/nova/internal/controller/reconcile_database.go`. Every other suite
installs a release into empty schemas and so only runs the steady-state db-sync.
A Keystone and a Placement stand beside the Nova, both at 2026.1 for the whole
run, because the expand phase runs `nova-status upgrade check` and that check
reads Placement as an authenticated client.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then bring up Keystone | `script` (2m) + `apply` + `assert` | `broker-vhost.sh create nova-upgrade nova-upgrade-messaging openstack` and `keystone-nova-upgrade` Ready |
| 2 | Register compute and placement in the catalog | `script` + `assert` | `01-catalog-setup-job.yaml`, `succeeded: 1` |
| 3 | Bring up the Placement the upgrade check reads | `apply` + `assert` | `placement-nova-upgrade` Ready |
| 4 | Assert the pre-upgrade steady state on :2025.2 | `apply` + `assert` (10m) + `error` | `installedRelease: "2025.2"`, `Ready=True/AllReady`, the API container on `ghcr.io/c5c3/nova:2025.2`, and `error` assertions on all three phase Jobs, which is what makes their presence in step 6 evidence of the upgrade |
| 5 | Patch the release and follow the phase to the end | `patch` + `script` (10m) + `assert` | The loop records each distinct `status.upgradePhase` it reads until the phase is empty and `installedRelease` is 2026.1, then checks the recorded values are an ordered subsequence of Expanding, Migrating, RollingUpdate, Contracting containing RollingUpdate |
| 6 | Assert the three phase Jobs ran on the new release | `assert` + `script` (2m) | `nova-upgrade-db-expand`, `-db-migrate` and `-db-contract` each `succeeded: 1` on `ghcr.io/c5c3/nova:2026.1`, and the expand pod's termination message reading `nova-status upgrade check exit 0` or `exit 1` |
| 7 | Assert all five Deployments rolled onto :2026.1 | `assert` + `script` (5m) | Every Deployment on the new image with `updatedReplicas == replicas`; the metadata API, scheduler, conductor and console proxy carry `nova.c5c3.io/installed-release: "2026.1"` and the API carries no such annotation, because it rolled during RollingUpdate. A script then waits for the steady-state db-sync Job to re-run on `:2026.1` and succeed |
| 8 | Assert the cells survived and the upgraded API answers | `assert` + `script` (2m) | cell0 still under the all-zero uuid and exactly one cell1, then a `GET /` from inside the conductor pod answering 200 with `v2.1` in the body |

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-placement-cr.yaml`, `03-metadata-secret.yaml`, `04-nova-cr.yaml`,
`05-patch-upgrade.yaml`

---

### compute-node-pool

**File:** `tests/e2e/nova/compute-node-pool/chainsaw-test.yaml`

**Purpose:** The lifecycle of a [NovaCompute](../nova/novacompute-crd.md) node
pool against a real Nova. `pool-a` runs `nova-compute` on the kind node through
the fake driver, which `spec.extraConfig` switches on (kind has no KVM), so the
suite needs no nested virtualization. The stack is the full-stack tier of
`basic-deployment` under the `nova-pool` prefix, plus a single-node OVNChassis
for the gate the pod waits on.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Label the node | `script` | `openstack.c5c3.io/chassis=true`, `topology.kubernetes.io/zone=nova-pool-az1` and `openstack.c5c3.io/nova-compute-pool=a`, and the ConfigMap `nova-pool-verify` naming the node. The step cleanup removes all three labels |
| 2 | Bring up the stack and the chassis | `script` (25m) | The vhost, `keystone-nova-pool`, the catalog Job, the four sibling CRs, the image seed, `nova-pool` up to `Ready`, and `nova-pool-chassis` up to `Ready`. The step cleanup tears the stack down |
| 3 | Apply pool-a | `script`, `assert`, `script` | `Ready=True/AllReady`; `status.nodes[0]` is `Active` in `nova-pool-az1` with the service `enabled`/`up`; `installedImage` is `ghcr.io/c5c3/nova-compute:2025.2`; the pod runs that image privileged as uid 0 and its `wait-for-chassis` init container exited 0; the verify Job (`registered`) finds the service and both aggregates with the marker `c5c3.io:nova=openstack/nova-pool`. The step cleanup deletes the pools and strips the drain finalizer from a survivor |
| 4 | A second pool on the same node | `script`, `assert` | `pool-b` selects the chassis label: `NodesReady=False/NodeConflict`, `status.nodes[0]` in `Conflict` with `pool-a`, no pod scheduled, and `pool-a` still Active. `pool-b` is then deleted, so `pool-a` is the last pool of the Nova |
| 5, 6 | Map the host and boot a server | `script` (10m) | `../discover-hosts.sh nova-pool "$NAMESPACE" "$NODE"`, then `15-boot-server-job.yaml` boots `s1` with no availability zone (the hypervisor operator does not run on kind, so the host never joins `nova-pool-az1`) and checks it landed on the node. Sentinel `NOVA-POOL-BOOT-OK` |
| 7 | Remove the pool label | `script`, `assert` | `status.nodes[0]` goes `Draining` with `instances: 1`, the service `disabled` with the reason `c5c3.io: leaving NovaCompute openstack/pool-a`, `ServicesReady=True/Draining`, and the pod still runs |
| 8 | Delete the server | `script`, `assert`, `script` | `16-delete-server-job.yaml` stands in for the hypervisor operator's eviction. `status.nodes` empties, and the verify Job (`released`) finds the service and `nova-pool-az1` gone and `tenant_filter_tests` still in place |
| 9 | Delete pool-a | `script`, `assert` | The pool leaves etcd, the verify Job (`torndown`) finds `tenant_filter_tests` gone, and `nova-pool-compute-config` stays, because it carries no mirror label |

**Fixtures:** `00`–`10` as in `basic-deployment` under the `nova-pool` names,
`11-ovnchassis-cr.yaml`, `12-novacompute-pool-a.yaml`,
`13-novacompute-pool-b.yaml`, `14-verify-job.yaml`, `15-boot-server-job.yaml`,
`16-delete-server-job.yaml`

**Design notes:**

- `concurrent: false`: the chassis and nova-compute pods own
  `/run/openvswitch` and `/var/lib/nova` on the one node.
- The pools are applied with `kubectl`, not as Chainsaw `apply` operations, and
  torn down in a step `cleanup` that runs before the stack's. A pool that is
  still draining would hold its finalizer for as long as Nova counts an
  instance, so the cleanup removes it by hand after three minutes rather than
  wedging the shared namespace.
- One verify Job serves three checks: the ConfigMap `nova-pool-verify` names
  the node and the state to expect. Sentinel `NOVA-POOL-VERIFY-OK`.

---

### console-proxy

**File:** `tests/e2e/nova/console-proxy/chainsaw-test.yaml`

**Purpose:** Three things a suite without a booted instance cannot show: the
console URL the API hands a client carries the gateway hostname the operator
rendered into `[vnc] novncproxy_base_url`, the token handshake through that
hostname reaches the console the fake driver reports
(`fakevncconsole.com:6969`), and a made-up token is turned down by the proxy.
The CR is `nova-vnc` rather than `nova-console`, because the webhook rejects a
Nova whose name ends in `-console`: `<name>-console` is the name of the console
proxy's HTTPRoute.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Bring the stack up and publish the console URL | `script` (25m) | Behind a presence guard for `GatewayClass/envoy`, `Gateway/openstack-gw`, the Nova CRD and the `https-nova-console` listener: the vhost, `keystone-nova-vnc`, the catalog Job, the messaging Secret and the four sibling CRs, the image-seed Job, `nova-vnc` up to `Ready` and `ConsoleHTTPRouteReady`, the fake compute, `../discover-hosts.sh nova-vnc`, and the verify Job to `NOVA-VERIFY-OK`. The `CONSOLE-URL=` line of that log is recorded in a Secret — the URL carries a live console token, and everything the step prints has it redacted. The step cleanup tears the whole stack down at the end of the test |
| 2 | The token handshake through the Gateway | `script` (5m) | Skips when the Secret is absent. The URL has to start with `https://nova-console.127-0-0-1.nip.io/vnc_lite.html?path=%3Ftoken%3D`; the noVNC page answers 200, the WebSocket upgrade on `/websockify` is switched to 101, and the proxy log carries `connect info:` and `connecting to: fakevncconsole.com:6969`. An all-zero token is switched to 101 as well and the log then reads `is invalid or has expired`. Sentinel `CONSOLE-OK` |
| 3 | Delete the server the handshake ran against | `script` (5m) | `13-delete-server-job.yaml` to `DELETE-OK` |

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-messaging-secret.yaml`, `03-ovncentral-cr.yaml`, `04-neutron-cr.yaml`,
`05-placement-cr.yaml`, `06-glance-cr.yaml`, `07-glancebackend-cr.yaml`,
`08-image-seed-job.yaml`, `09-metadata-secret.yaml`, `10-nova-cr.yaml`,
`11-fake-compute.yaml`, `12-verify-job.yaml`, `13-delete-server-job.yaml`

**Design notes:**

- The teardown is a step `cleanup`, not a `finally`. A per-step `finally` runs
  as soon as its own step ends, which would take the stack down between the
  bring-up and the handshake.
- `nova-console.127-0-0-1.nip.io` is a single listener on the one kind Gateway
  and `gateway-quick-start-smoke` claims it too. Two HTTPRoutes on one hostname
  are resolved by creation timestamp, so the catch block lists every route on
  the hostname.

---

### remote-compute-contract

**File:** `tests/e2e/nova/remote-compute-contract/chainsaw-test.yaml`

**Purpose:** The remote compute contract as a live API server stores it. A Nova
with a verified bus and `spec.remoteCompute` publishes
`nova-rc-remote-compute-config` beside `nova-rc-compute-config`. The suite
decodes both Secrets and checks each expected line in the INI section it belongs
to. No broker, Keystone or database runs for it: the brownfield database host,
the cache servers, the in-cluster bus URL and the external listener are
placeholders, because the ComputeConfig step runs before the Database step and
nothing before it dials them. The db-sync Job the Database step creates against
the placeholder host goes with the Nova at cleanup.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the inputs, then the CR | `apply` | The seven input Secrets, then `nova-rc` with Cinder and Barbican enabled and `remoteCompute` naming `https://keystone.example.test/v3` and `nova-rc-remote-messaging` |
| 2 | Assert the condition and both status refs | `assert` (3m) | `ComputeConfigReady=True/ComputeConfigPublished`, `status.computeConfigSecretRef.name: nova-rc-compute-config`, `status.remoteComputeConfigSecretRef.name: nova-rc-remote-compute-config` |
| 3 | Assert the content of both contracts | `script` (1m) | The remote Secret has six keys and `transport_url` is `rabbit://nova:nova@198.51.100.10:5671/`. The remote fragment has `auth_url = https://keystone.example.test/v3` in `[keystone_authtoken]` and `[service_user]`, `valid_interfaces = public` in `[placement]`, `[neutron]` and `[glance]`, `catalog_info = block-storage:cinder:publicURL`, `barbican_endpoint_type = public` and `ssl_ca_file = /etc/nova/compute-config/ca.crt`. The in-cluster fragment keeps its `auth_url` and `valid_interfaces = internal` |
| 4 | Remove `spec.remoteCompute` | `script` + `error` + `script` | A JSON patch removes the block; the remote Secret is gone, and a script polls until `status.remoteComputeConfigSecretRef` prints empty, since a JMESPath expression on the absent field would abort the assertion |

**Fixtures:** `00-secrets.yaml`, `01-nova-cr.yaml`

---

### invalid-cr

**File:** `tests/e2e/nova/invalid-cr/chainsaw-test.yaml`

**Purpose:** The rejection corpus. Each step applies an otherwise-valid Nova CR
with a single aspect violating the rule under test, captures the admission
`$error` and asserts the field path plus a message substring. The API server
validates the structural schema and its CEL rules before it calls the webhook,
so a rule carried by both layers is answered by the schema and the assertion
quotes the schema message; the webhook message is asserted where the rule has no
schema counterpart. The suite runs in an ephemeral namespace, and every fixture
is generated by `_generate.py`.

**Steps:**

| # | Fixture | Rejection asserted |
| --- | --- | --- |
| 1 | `00-openstackrelease-pattern.yaml` | `openStackRelease` … `should match` |
| 2 | `01-image-tag-and-digest.yaml` | `image` … `exactly one of image.tag or image.digest` |
| 3 | `02-apidatabase-clusterref-and-host.yaml` | `apiDatabase` … `exactly one of clusterRef or host` |
| 4 | `03-database-clusterref-and-host.yaml` | `database` … `exactly one of clusterRef or host` |
| 5 | `04-cache-clusterref-and-servers.yaml` | `cache` … `exactly one of clusterRef or servers` |
| 6 | `05-messaging-clusterref-and-secretref.yaml` | `messaging` … `exactly one of clusterRef or secretRef` |
| 7 | `06-messaging-neither-mode.yaml` | `messaging` … `exactly one of clusterRef or secretRef` |
| 8 | `07-messaging-tls-without-cabundle-name.yaml` | `messaging.tls.caBundleSecretRef.name` … `should be at least 1 chars long` |
| 9 | `08-messaging-missing.yaml` | `spec.messaging` … `exactly one of clusterRef or secretRef` |
| 10 | `09-dbarchive-schedule-invalid.yaml` | `dbArchive.schedule` … `invalid cron expression` |
| 11 | `10-dbarchive-maxrows-below-minimum.yaml` | `dbArchive.maxRows` … `should be greater than or equal to 1` |
| 12 | `11-dbarchive-retentiondays-below-minimum.yaml` | `dbArchive.retentionDays` … `should be greater than or equal to 1` |
| 13 | `12-scheduler-workers-below-minimum.yaml` | `scheduler.workers` … `should be greater than or equal to 1` |
| 14 | `13-databases-same-schema.yaml` | `apiDatabase and database must name different schemas` |
| 15 | `14-databases-credentialsmode-mismatch.yaml` | `apiDatabase and database must use the same credentialsMode` |
| 16 | `15-metadata-sharedsecretref-empty-name.yaml` | `metadata.sharedSecretRef.name` … `should be at least 1 chars long` |
| 17 | `16-endpoints-override-not-url.yaml` | `endpoints.placement.override` … `should match` |
| 18 | `17-extraconfig-rejected-owned-key.yaml` | `OS_DATABASE__CONNECTION` … `connection is managed via spec.database` |
| 19 | `18-extraconfig-unknown-option.yaml` | `no such option in the nova 2025.2 option catalog` |
| 20 | `19-name-too-long.yaml` | `metadata.name` … `name must be at most 41 characters` |
| 21 | `20-targetclusterref-empty-name.yaml` | `targetClusterRef.name` … `should be at least 1 chars long` |
| 22 | `21-dbarchive-sleep-below-minimum.yaml` | `dbArchive.sleep` … `should be greater than or equal to 0` |
| 23 | `22-keystoneendpoint-not-url.yaml` | `keystoneEndpoint` … `should match` |
| 24 | `23-apidatabase-names-cell0.yaml` | `apiDatabase must not name the cell0 schema derived from database` |
| 25 | `24-database-name-leaves-no-room-for-cell0.yaml` | `database.database must be at most 58 characters` |
| 26 | `25-name-collides-with-sibling-child.yaml` | `metadata.name` … `so the two CRs would share and delete each other` |
| 27 | `26-region-control-chars.yaml` | `spec.region` … `value must not contain a newline or carriage return` |
| 28 | `27-consoleproxy-gateway-path.yaml` | `consoleProxy.gateway.path` … `the console page and its WebSocket are served from the root of the console hostname` |
| 29 | `28-remotecompute-without-messaging-tls.yaml` | `remoteCompute requires messaging.tls` |
| 30 | `29-remotecompute-keystoneendpoint-not-url.yaml` | `remoteCompute.keystoneEndpoint` … `should match` |
| 31 | `30-remotecompute-keystoneendpoint-plaintext.yaml` | `remoteCompute.keystoneEndpoint` … `^https://` |
| 32 | `31-api-deployment-nodeselector-invalid-key.yaml` | `spec.api.deployment.nodeSelector` … `Invalid value` |
| 33 | `32-jobs-resources-request-above-limit.yaml` | `spec.jobs.resources.requests.memory` … `Invalid value` … `memory request must not exceed limit` |
| 34 | `33-api-deployment-toleration-empty-key-equal.yaml` | `spec.api.deployment.tolerations[0].operator` … `Invalid value` … `operator must be Exists when` |
| 35 | `34-autoscaling-cpu-request-zero.yaml` | `spec.api.deployment.resources.requests.cpu` … `cpu request must be greater than zero` |
| 36 | `35-autoscaling-behavior-window-above-max.yaml` | `spec.autoscaling.behavior.scaleDown.stabilizationWindowSeconds` … `stabilizationWindowSeconds must be between 0 and 3600` |
| 37 | `36-autoscaling-memory-target-unreachable.yaml` | `spec.autoscaling.targetMemoryUtilization` … `can never be reached` |

**Fixtures:** `_generate.py` and the 37 numbered fixtures above.
`make verify-invalid-cr-fixtures` runs `_generate.py --check` and
`test_generate.py`, so a hand-edited fixture fails the build before the
cluster-bound job runs.

---

### invalid-novacompute-cr

**File:** `tests/e2e/nova/invalid-novacompute-cr/chainsaw-test.yaml`

**Purpose:** The `NovaCompute` rejection corpus, on the pattern of `invalid-cr`.
The fixtures name a Nova that does not exist in the ephemeral namespace, which
admission tolerates: the extraConfig catalog check is then skipped with a
warning, so it never competes with the rule a fixture pins.

**Steps:**

| # | Fixture | Rejection asserted |
| --- | --- | --- |
| 1 | `00-novaref-name-empty.yaml` | `novaRef.name` … `should be at least 1 chars long` |
| 2 | `01-nodeselector-empty.yaml` | `nodeSelector` … `should have at least 1 properties` |
| 3 | `02-name-too-long.yaml` | `metadata.name` … `name must be at most 63 characters` |
| 4 | `03-targetclusterref-empty-name.yaml` | `targetClusterRef.name` … `should be at least 1 chars long` |
| 5 | `04-toleration-offboarding.yaml` | `spec.tolerations[0]` … `tolerates kvm.cloud.sap/offboarding:NoExecute` |
| 6 | `05-toleration-wildcard.yaml` | `spec.tolerations[0]` … `tolerates kvm.cloud.sap/offboarding:NoExecute` |
| 7 | `06-cpumodels-without-custom.yaml` | `spec.libvirt` … `cpuModels is required when cpuMode is custom and must be empty otherwise` |
| 8 | `07-custom-without-cpumodels.yaml` | the same message |
| 9 | `08-virttype-invalid.yaml` | `spec.libvirt.virtType` … `Unsupported value` |
| 10 | `09-imagestype-rbd.yaml` | `spec.libvirt.imagesType` … `Unsupported value` |
| 11 | `10-extraconfig-rejected-host.yaml` | `spec.extraConfig[DEFAULT][host]` … `must not be set in extraConfig` |
| 12 | `11-maxunavailable-with-ondelete.yaml` | `updateStrategy.maxUnavailable` … `maxUnavailable applies to RollingUpdate only` |
| 13 | `12-image-tag-and-digest.yaml` | `spec.image` … `exactly one of image.tag or image.digest must be set` |

**Fixtures:** `_generate.py` and the 13 numbered fixtures above, checked by
`make verify-invalid-cr-fixtures`.

---

### metrics

**File:** `tests/e2e/nova-operator/metrics/chainsaw-test.yaml`

**Purpose:** The nova-operator chart renders, and later removes, a
`monitoring.coreos.com/v1` ServiceMonitor pointing at the operator's `/metrics`
endpoint when `monitoring.serviceMonitor.enabled` is toggled.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Install nova-operator chart with monitoring enabled | `script` (120s) | `helm install nova-operator-metrics operators/nova/helm/nova-operator/ -n nova-system` with `webhook.enabled=false`, `replicas=1`, `monitoring.serviceMonitor.enabled=true` and `interval=30s` |
| 2 | Assert the ServiceMonitor exists with the expected spec | `assert` (5m) | ServiceMonitor `nova-operator-metrics` in `nova-system` with the chart's name, instance and managed-by labels, the matching selector, and one endpoint on port `metrics`, path `/metrics`, interval `30s` |
| 3 | Uninstall the test release (cleanup) | `script` (120s) | `helm uninstall nova-operator-metrics` |
| 4 | Assert the ServiceMonitor is removed | `script` | Polls up to a minute until `kubectl get servicemonitor nova-operator-metrics` fails |

**Fixtures:** none. The suite drives the Helm chart directly.

**Design notes:**

- The suite installs a release of its own rather than patching the cluster's
  operator. The `nova-system/nova-operator` HelmRelease is suspended in the kind
  CI overlay, and the operator that runs there is installed by
  `hack/ci-deploy-operator.sh`.
- `webhook.enabled=false` keeps the second release from registering a
  cluster-scoped webhook pair that would send Nova admission at a Service this
  release tears down again.
- Prometheus target-Up polling is out of scope. The E2E cluster installs
  prometheus-operator-crds but runs no Prometheus, so there is no
  `/api/v1/targets` endpoint to poll.

---

## Assertion Patterns

### Resource assertion (`assert`)

Declarative YAML matching against a Kubernetes resource, with JMESPath filter
syntax for conditions and for fields a plain subtree cannot reach. Used for
condition checks, replica counts, rendered container commands and resource
existence.

```yaml
- try:
    - assert:
        resource:
          apiVersion: nova.openstack.c5c3.io/v1alpha1
          kind: Nova
          metadata:
            name: nova-basic
            namespace: openstack
          status:
            installedRelease: "2025.2"
            (cells[?name == 'cell0'] || `[]`):
            - uuid: 00000000-0000-0000-0000-000000000000
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

A filter over a field that is still absent under an existing `status` fails
outright instead of retrying, so `status.cells` and `metadata.finalizers` are
read through a fallback to an empty list. `error` assertions carry the opposite
claim and appear wherever something must be absent: the two NetworkPolicies
after the patch in `network-policy`, every owned child in `deletion-cleanup`,
the MariaDB CRs in the test namespace in `pod-security-restricted`, and the
three phase Jobs before the upgrade in `release-upgrade`.

### The rendered configuration through its mount (`script`)

The config ConfigMap carries a content-hash suffix, so a suite resolves it
through the API Deployment's `config` volume rather than guessing the name, and
matches whole lines:

```bash
CM="$(kubectl get deploy nova-basic -n "$NAMESPACE" \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')"
kubectl get cm "$CM" -n "$NAMESPACE" -o 'jsonpath={.data.nova\.conf}' > /tmp/nova-basic.conf
grep -Fxq 'discover_hosts_in_cells_interval = 300' /tmp/nova-basic.conf
```

`valid_interfaces` appears in several client sections, so `[placement]` is cut
out with awk before the line is matched. The same pattern reads
`nova-compute.conf` out of the compute-config Secret, where the assertions run
both ways: the client sections a hypervisor needs are present, and `[database]`
and `[api_database]` are not.

### A request from a pod of the deployment (`script` with `kubectl exec`)

Anything the API has to answer is driven from a pod that is already there, so
the request crosses kube-proxy like any other client's and no extra pod is
scheduled on a node that already carries the suite. `release-upgrade` runs its
version-document check from the conductor pod, which carries the same image and
so has the interpreter:

```bash
kubectl exec -n "$NAMESPACE" deploy/nova-upgrade-conductor -c conductor -- \
  /var/lib/openstack/bin/python -c "..."
```

Connection-level failures are retried inside the pod, because kube-proxy's
endpoint programming can trail a rollout by a second or two. An HTTP status is a
verdict and fails hard.

### Host mapping through the conductor (`script`)

`../discover-hosts.sh <nova> <namespace> [host]` runs `nova-manage cell_v2
discover_hosts --verbose` and then reads `cell_v2 list_hosts` in the conductor
pod, the one workload that holds both database connections, and repeats the
pair until the host shows up. The third argument names the host to wait for and
defaults to `fake-1`, the compute the suites deploy. The nova tempest legs pass
the kind node's name: the OVN chassis registers under that name, and neutron
binds a port only to a host that has a live chassis. `list_hosts` prints
a prettytable, so the hostname is taken out of field 4 with awk, and the column
is captured before it is matched: a `... | grep -q` pipeline fails under
`pipefail` when it matches, because grep closes the pipe first. The two failure
paths are kept apart, since a `discover_hosts` that exits non-zero means the
command never reached the databases while an empty `list_hosts` means the
compute never registered.

The two `basic-deployment` suites use neither the helper nor a hand-run
discovery. They poll the same table for up to 420 seconds and then grep
`Discovered 1 new hosts` out of the scheduler log, which is what says the
periodic did the work.

### A verify Job on the tempest image (`script`)

Everything that drives the `openstack` client runs as a Job on
`ghcr.io/c5c3/tempest:2025.2` with `backoffLimit: 0` and
`restartPolicy: Never`: the scripts are not idempotent, so a retry would find
the server the first attempt created and fail on the name, and the failed pod is
the one whose log carries the verdict. Inside,
`wait_status <kind> <name> <want> <timeout>` polls one resource until it reports
the status the suite wants, and `wait_gone` polls until a show of it fails. The step around the Job deletes any
leftover of the same name (a Job's pod template is immutable), waits for
completion, reads the log once and greps the sentinel out of the captured text:

```bash
kubectl wait --for=condition=complete job/nova-basic-verify -n "$NAMESPACE" --timeout=10m
LOGS="$(kubectl logs -n "$NAMESPACE" job/nova-basic-verify)"
echo "$LOGS"
grep -q 'NOVA-VERIFY-OK' <<<"$LOGS"
```

The sentinels are per Job: `NOVA-VERIFY-OK` in the four suites that boot a
server and `DELETE-OK` in the console-proxy teardown Job, and
`NOVA-POOL-VERIFY-OK`, `NOVA-POOL-BOOT-OK` and `NOVA-POOL-DELETE-OK` in
`compute-node-pool`. Script steps that drive no Job print one of their own,
`CONSOLE-OK` and `ISOLATION-OK`.

## File Layout

```text
tests/e2e/nova/
├── discover-hosts.sh                  Map a compute into cell1 and wait for the mapping
├── basic-deployment/
│   ├── chainsaw-test.yaml             Happy path on 2025.2 with a booted server
│   ├── 00-keystone-cr.yaml            Keystone keystone-nova-basic
│   ├── 01-catalog-setup-job.yaml      Compute, placement, image and network catalog rows
│   ├── 02-messaging-secret.yaml       Transport URL for the Neutron beside the Nova
│   ├── 03-ovncentral-cr.yaml          OVNCentral nova-basic-ovn
│   ├── 04-neutron-cr.yaml             Neutron neutron-nova-basic
│   ├── 05-placement-cr.yaml           Placement placement-nova-basic
│   ├── 06-glance-cr.yaml              Glance glance-nova-basic
│   ├── 07-glancebackend-cr.yaml       S3 backend glance-nova-basic-s3
│   ├── 08-image-seed-job.yaml         The image the server boots from, under a fixed id
│   ├── 09-metadata-secret.yaml        The metadata shared secret
│   ├── 10-nova-cr.yaml                Nova CR nova-basic
│   ├── 11-fake-compute.yaml           nova-compute on the fake driver, host fake-1
│   └── 12-verify-job.yaml             Boot, resize and delete a server (NOVA-VERIFY-OK)
├── basic-deployment-2026-1/
│   ├── chainsaw-test.yaml             The same run on 2026.1
│   ├── 00-keystone-cr.yaml            Keystone keystone-nova-basic-2026-1
│   ├── 01-catalog-setup-job.yaml      Catalog rows for this suite's services
│   ├── 02-messaging-secret.yaml       Transport URL for the Neutron beside the Nova
│   ├── 03-ovncentral-cr.yaml          OVNCentral nova-basic-2026-1-ovn
│   ├── 04-neutron-cr.yaml             Neutron neutron-nova-basic-2026-1
│   ├── 05-placement-cr.yaml           Placement placement-nova-basic-2026-1
│   ├── 06-glance-cr.yaml              Glance glance-nova-basic-2026-1
│   ├── 07-glancebackend-cr.yaml       S3 backend glance-nova-basic-2026-1-s3
│   ├── 08-image-seed-job.yaml         The seed image of this suite
│   ├── 09-metadata-secret.yaml        The metadata shared secret
│   ├── 10-nova-cr.yaml                Nova CR nova-basic-2026-1 on 2026.1
│   ├── 11-fake-compute.yaml           Fake-driver compute for this suite
│   └── 12-verify-job.yaml             Boot, resize and delete a server
├── compute-node-pool/
│   ├── chainsaw-test.yaml             A NovaCompute node pool from registration to teardown
│   ├── 00-…-10-….yaml                 The basic-deployment stack under the nova-pool names
│   ├── 11-ovnchassis-cr.yaml          OVNChassis nova-pool-chassis on the labelled node
│   ├── 12-novacompute-pool-a.yaml     NovaCompute pool-a on the fake driver
│   ├── 13-novacompute-pool-b.yaml     NovaCompute pool-b selecting the same node
│   ├── 14-verify-job.yaml             The service and the aggregates (NOVA-POOL-VERIFY-OK)
│   ├── 15-boot-server-job.yaml        Boot s1 onto the pool's node (NOVA-POOL-BOOT-OK)
│   └── 16-delete-server-job.yaml      Delete s1 (NOVA-POOL-DELETE-OK)
├── console-proxy/
│   ├── chainsaw-test.yaml             Console URL, token handshake and rejection
│   ├── 00-keystone-cr.yaml            Keystone keystone-nova-vnc
│   ├── 01-catalog-setup-job.yaml      Catalog rows for this suite's services
│   ├── 02-messaging-secret.yaml       Transport URL for the Neutron beside the Nova
│   ├── 03-ovncentral-cr.yaml          OVNCentral nova-vnc-ovn
│   ├── 04-neutron-cr.yaml             Neutron neutron-nova-vnc
│   ├── 05-placement-cr.yaml           Placement placement-nova-vnc
│   ├── 06-glance-cr.yaml              Glance glance-nova-vnc
│   ├── 07-glancebackend-cr.yaml       S3 backend glance-nova-vnc-s3
│   ├── 08-image-seed-job.yaml         The seed image of this suite
│   ├── 09-metadata-secret.yaml        The metadata shared secret
│   ├── 10-nova-cr.yaml                Nova CR nova-vnc with spec.consoleProxy.gateway
│   ├── 11-fake-compute.yaml           Fake-driver compute for this suite
│   ├── 12-verify-job.yaml             Boot a server and print its console URL
│   └── 13-delete-server-job.yaml      Delete that server again (DELETE-OK)
├── db-archive/
│   ├── chainsaw-test.yaml             The archive CronJob moves rows to the shadow tables
│   ├── 00-keystone-cr.yaml            Keystone keystone-nova-archive
│   ├── 01-catalog-setup-job.yaml      Catalog rows for this suite's services
│   ├── 02-messaging-secret.yaml       Transport URL for the Neutron beside the Nova
│   ├── 03-ovncentral-cr.yaml          OVNCentral nova-archive-ovn
│   ├── 04-neutron-cr.yaml             Neutron neutron-nova-archive
│   ├── 05-placement-cr.yaml           Placement placement-nova-archive
│   ├── 06-glance-cr.yaml              Glance glance-nova-archive
│   ├── 07-glancebackend-cr.yaml       S3 backend glance-nova-archive-s3
│   ├── 08-image-seed-job.yaml         The seed image of this suite
│   ├── 09-metadata-secret.yaml        The metadata shared secret
│   ├── 10-nova-cr.yaml                Nova CR nova-archive on a minute schedule
│   ├── 11-fake-compute.yaml           Fake-driver compute for this suite
│   └── 12-verify-job.yaml             Boot and delete a server, leaving the backlog
├── deletion-cleanup/
│   ├── chainsaw-test.yaml             Finalizer cleanup of every child and the MariaDB CRs
│   ├── 00-metadata-secret.yaml        The metadata shared secret
│   └── 01-nova-cr.yaml                Nova CR nova-cleanup
├── gateway-quick-start-smoke/
│   ├── chainsaw-test.yaml             The three external URLs, SKIP-gated
│   ├── 00-namespace.yaml              The namespace the smoke run labels
│   ├── 01-metadata-secret.yaml        The metadata shared secret
│   └── 02-smoke-nova-cr.yaml          Nova CR nova-smoke with the three gateway blocks
├── healthcheck/
│   ├── chainsaw-test.yaml             NovaAPIReady and status.endpoint
│   ├── 00-metadata-secret.yaml        The metadata shared secret
│   └── 01-nova-cr.yaml                Nova CR nova-health
├── httproute/
│   ├── chainsaw-test.yaml             The three gateway blocks through their lifecycle
│   ├── 00-httproute-crd.yaml          Minimal HTTPRoute CRD for clusters with no Gateway controller
│   ├── 01-metadata-secret.yaml        The metadata shared secret
│   ├── 02-nova-cr.yaml                Nova CR nova-route with all three gateway blocks
│   └── 03-patch-remove-gateways.yaml  Patch unsetting the three blocks
├── invalid-cr/
│   ├── chainsaw-test.yaml             Nova rejection corpus
│   ├── _generate.py                   Generator for the fixtures below
│   ├── test_generate.py               Unit test of the generator
│   └── 00-…-30-….yaml                 Thirty-one rejection fixtures
├── invalid-novacompute-cr/
│   ├── chainsaw-test.yaml             NovaCompute rejection corpus
│   ├── _generate.py                   Generator for the fixtures below
│   ├── test_generate.py               Unit test of the generator
│   └── 00-…-12-….yaml                 Thirteen rejection fixtures
├── maintenance-endpoint-isolation/
│   ├── chainsaw-test.yaml             The archive pod stays out of the three EndpointSlices
│   ├── 00-metadata-secret.yaml        The metadata shared secret
│   └── 01-nova-cr.yaml                Nova CR nova-isolation with two API replicas
├── network-policy/
│   ├── chainsaw-test.yaml             The two policies, created, updated and deleted
│   ├── 00-metadata-secret.yaml        The metadata shared secret
│   ├── 01-nova-cr.yaml                Nova CR nova-netpol
│   ├── 02-patch-update-ingress.yaml   Patch adding a second ingress source
│   └── 03-patch-disable-networkpolicy.yaml Patch removing spec.networkPolicy
├── pod-security-restricted/
│   ├── chainsaw-test.yaml             Reconciliation under the restricted PSS profile
│   ├── 00-namespace.yaml              Labelled namespace nova-pss and its two ExternalSecrets
│   ├── 00-brownfield-db-setup.yaml    Pre-created MariaDB Databases, Users and Grants
│   └── 01-nova-cr.yaml                Nova CR nova-pss in brownfield mode
├── release-upgrade/
│   ├── chainsaw-test.yaml             Cross-release upgrade 2025.2 to 2026.1
│   ├── 00-keystone-cr.yaml            Keystone keystone-nova-upgrade
│   ├── 01-catalog-setup-job.yaml      Compute and placement catalog rows
│   ├── 02-placement-cr.yaml           Placement placement-nova-upgrade
│   ├── 03-metadata-secret.yaml        The metadata shared secret
│   ├── 04-nova-cr.yaml                Nova CR nova-upgrade on 2025.2
│   └── 05-patch-upgrade.yaml          Patch to release and image tag 2026.1
├── remote-compute-contract/
│   ├── chainsaw-test.yaml             Both compute contracts, and the remote one's removal
│   ├── 00-secrets.yaml                The seven input Secrets, every address a placeholder
│   └── 01-nova-cr.yaml                Nova CR nova-rc with spec.remoteCompute
└── scale/
    ├── chainsaw-test.yaml             API replica scaling and PDB policy
    ├── 00-metadata-secret.yaml        The metadata shared secret
    ├── 01-nova-cr.yaml                Nova CR nova-scale with three API replicas
    ├── 02-patch-scale-up.yaml         Patch to 5 replicas
    └── 03-patch-scale-to-one.yaml     Patch to 1 replica

tests/e2e/nova-operator/
└── metrics/
    └── chainsaw-test.yaml             nova-operator chart ServiceMonitor
```

## Related Resources

- [Nova CRD](../nova/nova-crd.md): CRD types, webhooks and the rendered configuration
- [Nova Reconciler Architecture](../nova/nova-reconciler.md): sub-reconciler contracts and unit tests
- [Chaos E2E Test Suites](./chaos-e2e-tests.md): the three Nova outage suites on the chaos `nova` leg
- [CI Workflow](../ci-cd/ci-workflow.md): the `e2e-operator` matrix leg that runs these suites
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md): infrastructure stack deployment and `WITH_MESSAGING`
- `tests/e2e/chainsaw-config.yaml`: shared Chainsaw configuration
