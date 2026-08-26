---
title: Adding a New Operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adding a New Operator

This checklist captures everything a new service operator (e.g. Horizon)
touches beyond its own `operators/<op>/` module. The generic controller
scaffolding lives in `internal/common` — a new operator consumes it instead of
copying the keystone implementation — and the remainder is a finite list of
build/CI/config seams that are still enumerated per operator.

## Shared packages to consume

Build the operator on the shared scaffolding rather than hand-rolling copies.
The keystone operator is the reference consumer for most packages listed (for
`pysettings` it is the horizon operator).

| Package | Provides |
| --- | --- |
| `internal/common/types` | Shared CRD spec types (`DatabaseSpec`, `CacheSpec`, `GatewaySpec`, `ImageSpec`, `DeploymentSpec`, `AutoscalingSpec`, `NetworkPolicySpec`, `LoggingSpec`, ...) with their CEL rules and `Default()` methods |
| `internal/common/naming` | Label keys and `CommonLabels`/`SelectorLabels`, the workload naming convention (and the cross-service endpoint contract, see below); the sub-resource name is the bare CR name, so each operator keeps its own one-line helper |
| `internal/common/reconcile` | Table-driven pipeline (`Step`/`RunPipeline`), parallel groups (`ParallelStep`/`RunParallelGroup`), non-short-circuiting sequential groups (`RunSequentialGroup`), `ShortestRequeue`, `SetAggregateReady`, the no-op-skipping `UpdateStatus`, `EnsureFinalizer`, the shared requeue constants (`RequeueDeploymentPolling`/`RequeueSecretPolling`/`RequeueNextPass`), the `Skeleton[T,S]` controller-skeleton glue (Ready aggregation, status write, `MarkFailed`, parallel-group), and `ProjectChild`/`DeleteOrphanedChild` for orchestrating operators that project child CRs |
| `internal/common/conditions` | `SetCondition` (a `LastTransitionTime`-preserving upsert), `GetCondition`, `IsReady`, `AllTrue` |
| `internal/common/watch` | `CRUpdatePredicate` for the `For(...)` watch, `SecretToOwnersMapper` + `RegisterSecretNameIndex`, `StoreRefFanOut` (enqueues only the CRs whose effective `spec.secretStoreRef` matches a changed cluster-scoped `ClusterSecretStore` or namespaced `SecretStore`), `ClusterRefMapper` (database-cluster reference to owning CRs) |
| `internal/common/bootstrap` | `Run`/`ManagerConfig` manager bootstrap, `NewScheme` scheme assembly (client-go baseline first, then the per-operator extras), `ControllerOptions` (concurrency + tuned rate limiter), `DetectOperatorNamespace` |
| `internal/common/instrumentation` | Sub-reconciler duration/error metrics; declare a `NewSubReconcilerInstrumenter("<op>_operator", conditionTypes)` in the operator and pass its bound `Instrument` method to the reconcile pipeline, then register it from a `RegisterMetrics()` wired into `main.go` (registration returns an error instead of panicking) |
| `internal/common/deployment` | SSA ensure primitives, `BuildWorkload`/`BuildService` (shared pod-template and Service assembly), `BuildDaemonSet`/`EnsureDaemonSet` for node-level workloads, `RestrictedSecurityContext` beside the `CapabilitySecurityContext`/`PrivilegedSecurityContext` escapes, PDB/HPA builders, `ReconcileHPA` flow, replica normalization, pod-knob default helpers |
| `internal/common/apply` | The SSA apply primitives under every ensure helper: `EnsureObject` (owner-referenced) and `EnsureUnownedObject` |
| `internal/common/networkpolicy` | `Ensure`/`Delete`, the auto-derived egress rules (`DNSEgressRule`/`DatabaseEgressRule`/`CacheEgressRule`/`CacheEgressPorts`), `IngressPeers`, and the three-path `Reconcile` flow with the fail-closed empty-ingress guard |
| `internal/common/gateway` | `IsGVKAvailable` CRD probe, HTTPRoute builder/acceptance/ensure/delete over the shared `GatewaySpec`, and the three-path `ReconcileHTTPRoute` flow |
| `internal/common/secrets` | ESO primitives, `OpenBaoClusterStoreName`, per-CR store-ref resolution (`EffectiveStoreRef`, `IsStoreRefReady`, `ESOSecretStoreRef`, `PushSecretStoreRefs`), the `GateSyncedSecret` ladder, the `GateStoreReady` store-ref gate and `GateCredential`/`GateCredentials` condition-reporting loop |
| `internal/common/validation` | Shared webhook validators (DB/cache XOR, dynamic-credentials rule, cron parse, TSC selector, PriorityClass lookup) |
| `internal/common/webhook` | `NoopDeleteValidator` (the never-invoked `ValidateDelete` embed for webhooks that guard create and update only) |
| `internal/common/database`, `internal/common/cache` | MariaDB CR apply, `BuildDatabase`/`BuildUser`/`BuildGrant` provisioning builders, host/port/username resolution, pymysql DSN + TLS params + rollout digest, memcache server resolution; plus the reconcile flows layered on top: `ReconcileProvision` (cluster gate + Database/User/Grant ensure + Dynamic-credentials skip), `ReconcileSyncJobs` (db-sync + schema-check via the parameterized `JobSetParams` table, `InstalledRelease` tracking), `ReconcileConnectionSecret` + `ConnectionEnvVar`/`ConnectionSecretName` (derived `<name>-db-connection` Secret + DSN digest), and `FinalizeResources`/`HasLiveResources` finalizer cleanup |
| `internal/common/rotation` | Split-compute-write credential rotation: `EnsureStagingSecret`, `CommitStaged`/`CommitSpec`, `EnsureRBAC`, `CompletedAt`/`ObserveAge`, `BuildCronJob`/`CronJobParams` |
| `internal/common/tls` | `EnsureCertificate` for cert-manager Certificate objects |
| `internal/common/release` | OpenStack release parsing and upgrade/downgrade classification |
| `internal/common/healthcheck` | `HTTPDoer` seam, probe-error classifier, TTL probe cache, the shared timing constants, and the `ReconcileProbe` flow |
| `internal/common/job` | `RunJob`/`RunJobWithRerunKey`/`EnsureCronJob`/`DeleteCronJob`, the `BuildMigrationJob` skeleton, and the at-most-once `RecordJobTerminalState` recorder |
| `internal/common/config` | oslo INI rendering + immutable-ConfigMap lifecycle, the extraConfig overlay (`MergeDefaults`) with owned-key health tracking, and the option-catalog machinery (`ParseOptionCatalog`, `MustParseEmbeddedCatalogs`); see the design decisions below for non-INI services |
| `internal/common/keystoneauth` | `[keystone_authtoken]` section rendering without the password (`Section`) plus the `OS_KEYSTONE_AUTHTOKEN__PASSWORD` env-var injection (`PasswordEnvVar`) |
| `internal/common/policy` | oslo policy handling: `LoadPolicyFromConfigMap`, `RenderPolicyYAML`, `MergePolicies`, `ValidatePolicyRules` |
| `internal/common/plugins` | Paste-pipeline and middleware/plugin config rendering for services with a paste-deploy stack |
| `internal/common/pysettings` | Python-settings rendering for non-INI services (Django `local_settings.py`); see the design decisions below |
| `internal/common/testutil/envtest` | envtest bootstrap, `BuildScheme`, `CommonExternalSchemes`, `CommonFakeCRDDirs`, `StartManagedEnvTest`, `SetupEnvTestWithCRDs` (webhook-less), `SetupUnstartedManager` |

## Residual touch list

Everything below is still enumerated per operator. Work through it top to
bottom when scaffolding `operators/<op>/`:

- **`go.work`** — add `./operators/<op>`; keep the Go directive and the
  controller-runtime/k8s.io dependency versions in lockstep with the other
  modules (see [Dependency Management](./dependency-management.md)).
- **`operators/Dockerfile`** — the parameterized Dockerfile builds every
  operator via `--build-arg OPERATOR=<op>`; add the new module's
  `go.mod`/`go.sum` and source `COPY` lines (two lines total).
- **`Makefile`** — append the operator to the `OPERATORS ?=` list; every
  build/test/generate/lint target iterates it, provided the chart lives at
  `operators/<op>/helm/<op>-operator/`.
- **Helm chart** — scaffold `operators/<op>/helm/<op>-operator/` consuming the
  `operator-library` chart: every shared manifest (deployment, certificate,
  service, serviceaccount, rolebindings, PDB, ServiceMonitor,
  webhook-configuration) is a one-line `include`; per-operator content is
  `Chart.yaml`, `values.yaml`, the `<op>-operator.rbacRules` helper, and the
  helm-unittest suite.
- **`.gitignore`** — add `operators/<op>/helm/<op>-operator/charts/` (bare
  pattern, no leading slash) so the `operator-library` tarball vendored by
  `make helm-deps` stays untracked.
- **`hack/gen-helm-values-schema.py`** — charts are discovered from the
  directory layout; add the new chart's `WEBHOOK_ENABLED_DESCRIPTIONS` entry
  (the generator fails loudly without it), then run `make gen-helm-schema`.
- **Option catalog** (oslo-INI services only) — map the service to its
  upstream oslo-config-generator config in `hack/gen-option-catalog.sh`, add
  it to the service loops of the `gen-option-catalogs` and
  `verify-option-catalogs` Makefile targets, and commit the generated
  per-release catalogs under `operators/<op>/api/v1alpha1/catalogs/` with a
  `go:embed` accessor. The validating webhook checks `spec.extraConfig`
  against these catalogs; both targets need docker and the published service
  images.
- **`.github/workflows/ci.yaml`** — the biggest surface: add the operator to
  the paths-filter groups, `ALL_OPERATORS` and the `FILTER_<op>` env var, the
  unit/integration test matrices, the helm-validate chart loops, and the
  `build-e2e-images` operator resolution.
- **`.github/workflows/build-images.yaml`** — nothing to do for the operator
  image (the shared `operators/Dockerfile` is already wired); only new service
  images under `images/` need matrix entries.
- **`.github/workflows/cleanup-images.yaml`** — nothing to do. The GHCR
  cleanup jobs build their matrix from `hack/ci-generate-cleanup-matrix.sh`,
  which reads `images/` and `operators/`, so a new operator picks up its
  retention coverage from the directory you already created. The
  service-parity audit asserts the generator emits `<op>-operator` and
  `<op>`.
- **`.codecov.yml`** — add the per-operator `unit-<op>`/`integration-<op>`
  flag blocks; the components section auto-scales via `operators/*` globs.
- **Dashboards** — ship `operators/<op>/dashboards/<op>-operator.json` with a
  `dashboard_test.go` asserting the JSON parses and every metric a panel
  references is registered by the operator. Only the keystone dashboard is
  staged into the kind Prometheus stack; no deploy wiring is needed.
- **Tests** — `operators/<op>/internal/testutil/` wraps
  `internal/common/testutil/envtest` with the op-local CRD/webhook paths and
  scheme list; E2E suites live under `tests/e2e/<op>/` (one directory per
  feature, mirroring `tests/e2e/keystone/`), plus at least one chaos suite
  under `tests/e2e-chaos/`.
- **Deploy stack** — a flux HelmRelease under `deploy/flux-system/releases/`
  with the matching kind-overlay suspend patch, plus the literal lists in
  `hack/deploy-infra.sh` (the un-suspend step and the ServiceMonitor toggle)
  and the target tuple in `hack/refresh-operator-image-digests.sh`. An
  operator missing from the un-suspend list wedges the ControlPlane cache
  sync.
- **Docs** — a CRD reference and reconciler reference under
  `docs/reference/<op>/`, wired into `docs/.vitepress/config.ts`, plus a
  naming-convention test in `tests/unit/docs/` (discovered by glob, so
  nothing fails while it is missing).

## Design decisions the shared scaffolding encodes

Three cross-service decisions were settled when the scaffolding was extracted;
new operators build on them rather than reopening them:

- **Cross-service endpoint discovery is convention-based.** Consumers derive a
  service URL from the naming convention (`internal/common/naming`):
  `http://<name>.<namespace>.svc.cluster.local:<port>` over the Service, which
  is named after the CR itself. Keystone publishes `Status.Endpoint` for human
  consumers only; no machine consumer reads it, and no status-based resolve
  helper or cross-CR watch exists. If a new operator needs endpoint shapes the
  convention cannot express, build that helper then — not preemptively.
- **Periodic maintenance belongs to the operator, from the first release
  on.** OpenStack services expect somebody to run their housekeeping
  commands on a schedule — `glance-manage db purge`,
  `keystone-manage trust_flush`, cache pruning, expiry sweeps. A package
  deployment answers that with a cron entry on a controller node; a
  Kubernetes deployment has no such place, so a task the operator does not
  project as a CronJob never runs at all, silently: the workload stays
  Ready, every probe passes, and the only symptom is a table or a cache
  that keeps growing. Model each task in the spec (`spec.dbPurge` on
  Glance, `spec.trustFlush` on Keystone), project it from a sub-reconciler
  via `internal/common/job`'s `EnsureCronJob`, and give it a `<Task>Ready`
  condition that reports the newest terminal run rather than the mere
  existence of the CronJob. Scaffold this with the operator: the first
  CronJob added later also has to bound `metadata.name`, since Kubernetes
  caps a CronJob name at 52 characters and an immutable field can only be
  bounded on create — leaving the CRs admitted before the bound to a
  hash-collapse fallback in the controller.
- **Non-INI configuration rendering gets its own package.**
  `internal/common/config` renders oslo INI only and stays that way. A service
  that renders Python settings (e.g. Horizon's Django `local_settings.py`)
  gets a separate shared renderer package rather than bolting Python emission
  onto the INI renderer. `internal/common/pysettings` now exists, implemented
  with its first consumer, the horizon-operator.

## Node-level workloads

Every operator on this page projects API Deployments onto whatever node the
scheduler picks. A node-level workload does not work that way: the OVN chassis
(issue #903) runs one pod per node, needs kernel access the Restricted posture
forbids, and reads part of its configuration off the node it landed on. Issues
#903 and #905 implement against the rules below.

### Container posture

`RestrictedSecurityContext` stays the posture for every API Deployment, Job and
CronJob, guarded by the five `pod-security-restricted` suites under
`tests/e2e/<op>/`. `internal/common/deployment` has two escapes beside it,
added with the OVN chassis (#903), and a container takes the weaker one that
still works:

- `CapabilitySecurityContext(caps...)` keeps the Restricted fields and adds
  named capabilities. ovs-vswitchd and ovn-controller need `NET_ADMIN`;
  ovsdb-server needs no capability and stays Restricted.
- `PrivilegedSecurityContext()` is for a container that cannot run any other
  way: the `SYS_MODULE` init container that loads modules from the host's
  module tree, and the metadata agent, which reaches into `/run/netns`.

A DaemonSet that uses either one lives in a namespace labelled
`pod-security.kubernetes.io/enforce: privileged`. On a target cluster the label
comes from the access chart's `privilegedNamespaces` value, which also gates the
`daemonsets` grant, and which gives the chassis a namespace of its own: without
PodSecurity enforcement, `create` on a workload there reaches node root, so
nothing else may be placed in it. See
[Target Clusters](../reference/target-clusters.md) for what that costs. On a kind
devstack no label is needed: PodSecurity admission defaults to the `privileged`
level there, so an unlabelled namespace admits the pods as they are.

### Per-node values

Identity the kubelet already knows comes through the downward API: `NODE_NAME`
from `spec.nodeName`, `NODE_IP` from `status.hostIP`. An init container applies
it before the daemon starts.

```bash
ovs-vsctl set open . external_ids:hostname="$NODE_NAME" external_ids:ovn-encap-ip="$NODE_IP"
```

A value that depends on cluster state the pod cannot see (the gateway role, the
bridge mappings derived from node labels) travels in a ConfigMap. The operator
renders one per chassis, `<name>-nodes`, with a key per selected node holding
that node's `SYSTEM_ID=`, `GATEWAY=`, `BRIDGE_MAPPINGS=` and `ENCAP_TYPE=` as
shell assignments. The pod mounts it as a volume at `/etc/ovn-chassis/nodes`,
and the init container sources the key named after its own node.

Nothing in the pod calls the API server for this, so it needs no Role: the OVN
image carries no HTTP client to make the call with. A changed value reaches a
running pod anyway, because the kubelet refreshes a mounted ConfigMap within its
sync period.

Stable identity, the OVN `system-id`, has to survive the pod, so the operator
persists it — into that same namespaced per-node object, and it reads it back on
the next pass. A Node annotation would be the other candidate, and it is the
wrong one: writing it needs `patch` on `nodes`, Kubernetes cannot narrow that
verb to one annotation key, and the labels and taints it carries with it are
enough to lift `node-role.kubernetes.io/control-plane:NoSchedule` and put a
privileged chassis pod on the control-plane node. The access chart therefore
grants `nodes` read-only and has no `patch` opt-in at all. The chassis pod holds
no cluster-scoped grant of its own either, and the chart grants it no ClusterRole
or ClusterRoleBinding write.

### Placement and teardown

A DaemonSet placed on a target cluster is a remote child like any other. It
carries the ownership labels described under "Ownership and teardown on the
target" in [Target Clusters](../reference/target-clusters.md), so the
placed-namespace sweep reclaims it when the owning CR goes away.

### CI substrate

`KIND_CONFIG=hack/kind-config-multinode.yaml` on `make deploy-infra` brings up
one control-plane node plus two workers. The default single-node config binds
the same host ports, so only one of the two clusters can exist on a host at a
time. `WITH_OVN_KERNEL_MODULES=true` makes `hack/deploy-infra.sh` load
`openvswitch` and `geneve` on the host first; the `setup-e2e-infra` action
threads the flag through for CI. Which of those a job may block on is settled in
[Kernel-module-dependent suites](../reference/ci-cd/ci-workflow.md#kernel-module-dependent-suites).
