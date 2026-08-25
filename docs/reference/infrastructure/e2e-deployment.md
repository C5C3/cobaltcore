---
title: Infrastructure E2E Deployment
quadrant: infrastructure
---

# Infrastructure E2E Deployment

Reference documentation for the infrastructure E2E deployment automation.
This feature provides shell-based orchestration to deploy the full infrastructure stack
(cert-manager, OpenBao, ESO, MariaDB Operator, Memcached Operator, infrastructure CRs,
ExternalSecrets) into a local kind cluster and validate it with Chainsaw E2E tests.

## Architecture Overview

```text
┌─────────────────────────────────────────────────────────────────────────┐
│  Developer / CI Runner                                                  │
│                                                                         │
│  make install-test-deps   ──▶  Installs chainsaw, flux, kind, kubectl   │
│  make deploy-infra        ──▶  8-step deployment into kind cluster      │
│  make e2e                 ──▶  Chainsaw E2E tests against the cluster   │
│  make teardown-infra      ──▶  Deletes the kind cluster                 │
│                                                                         │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Kind Cluster (cobaltcore)                                               │
│                                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                   │
│  │ cert-manager │  │   OpenBao    │  │     ESO      │                   │
│  │  (Deployment)│  │ (StatefulSet)│  │ (Deployment) │                   │
│  └──────────────┘  └──────────────┘  └──────────────┘                   │
│  ┌──────────────┐  ┌──────────────┐                                     │
│  │   MariaDB    │  │  Memcached   │                                     │
│  │  Operator    │  │  Operator    │                                     │
│  │ (Deployment) │  │ (Deployment) │                                     │
│  └──────┬───────┘  └──────┬───────┘                                     │
│         │                 │                                             │
│  ┌──────▼───────┐  ┌──────▼───────┐  ┌──────────────────────┐           │
│  │  MariaDB CR  │  │ Memcached CR │  │ ClusterIssuer        │           │
│  │ (openstack-  │  │ (openstack-  │  │ (selfsigned-cluster- │           │
│  │  db)         │  │  memcached)  │  │  issuer)             │           │
│  └──────────────┘  └──────────────┘  └──────────────────────┘           │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────┐          │
│  │ ExternalSecrets: keystone-admin, keystone-db,             │          │
│  │                  mariadb-root-password                    │          │
│  └───────────────────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

| Prerequisite | Details |
| --- | --- |
| Docker | Running Docker daemon (kind uses Docker containers as nodes) |
| kubectl | Kubernetes CLI for cluster interaction |
| kind | Kubernetes IN Docker for local cluster creation |
| flux | **Optional** — the Flux CLI is no longer required by `make deploy-infra`; bootstrap uses flux-operator + FluxInstance. Opt in with `WITH_FLUX_CLI=true make install-test-deps` for ad-hoc `flux logs` debugging. |
| chainsaw | Kyverno Chainsaw for E2E test execution |
| jq | JSON processor used by deployment scripts |

All CLI tools except Docker can be installed via `make install-test-deps`.

## Makefile Targets

### `make deploy-infra`

Deploys the full infrastructure stack to a kind cluster by running
`hack/deploy-infra.sh`. The script executes an 8-step deployment sequence
(see [Deployment Sequence](#deployment-sequence) below). Exits 0 on success,
non-zero on any failure with a descriptive error message.

### `make teardown-infra`

Deletes the kind cluster by running `hack/teardown-infra.sh`. Idempotent —
succeeds silently if no cluster exists. Always exits 0.

### `make install-test-deps`

Installs pinned versions of chainsaw, flux, kind, and kubectl by running
`hack/install-test-deps.sh`. Idempotent — skips tools already installed at the
correct version. Installs to `$INSTALL_DIR` (default: `~/.local/bin`).

### `make e2e`

Runs all Chainsaw E2E tests: `chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/`.
Produces JUnit XML reports in `_output/reports/`.

## Deployment Sequence

`hack/deploy-infra.sh` implements the following 8-step sequence:

```text
Step 1 ── Create kind cluster (hack/kind-config.yaml)
     │
Step 2 ── Install flux-operator + apply FluxInstance
     │         kubectl apply -f flux-operator install.yaml
     │         kubectl apply -f deploy/flux-system/fluxinstance.yaml
     │         wait_for_fluxinstance polls Ready condition
     │
     ├── Install Gateway API standard CRDs
     │         kubectl apply --server-side -f <upstream standard-install.yaml>
     │         Required by the keystone-operator HTTPRoute watch; version
     │         pinned via GATEWAY_API_VERSION, default matches go.mod.
     │         Skipped when all ten standard-channel CRDs of the pinned
     │         bundle already exist at that bundle version, or at a newer
     │         or unversioned one (this step never downgrades); a complete
     │         live set OLDER than the pin is upgraded in place with the
     │         same server-side apply. The bundle also ships the
     │         safe-upgrades ValidatingAdmissionPolicy, which denies
     │         applying experimental-channel CRDs over the standard
     │         channel.
     │
     ├── Install Envoy Gateway CRDs (gateway.envoyproxy.io)
     │         kubectl apply --server-side -f <upstream envoy-gateway-crds.yaml>
     │         Version pinned via ENVOY_GATEWAY_VERSION, kept inside the
     │         envoy-gateway chart's SemVer range. The `envoy-gateway`
     │         HelmRelease runs with `crds.enabled: false` — its bundled
     │         CRD copy carries the experimental Gateway API channel,
     │         which the safe-upgrades policy above refuses over the
     │         standard pin — so this step is the only owner of the
     │         gateway.envoyproxy.io group. Skipped when all eight CRDs
     │         already exist (they carry no comparable version
     │         annotation, so present sets are never re-asserted).
     │
     ├── Install Envoy Gateway + Gateway/openstack-gw (kind-only)
     │         Installed as part of the deploy/kind/base/ overlay applied
     │         in Step 3: the `envoy-gateway` HelmRelease brings up the
     │         control plane, and deploy/kind/base/openstack-gateway.yaml
     │         creates GatewayClass/envoy (parametersRef → EnvoyProxy with
     │         NodePort 31443), a cert-manager Certificate for
     │         keystone.127-0-0-1.nip.io signed by selfsigned-cluster-issuer,
     │         and Gateway/openstack-gw on :443. wait_for_gateway_programmed
     │         polls Programmed=True after Phase 3.
     │         The production deploy/flux-system/ overlay does NOT ship
     │         these resources — operators pick their own Gateway
     │         implementation in production.
     │
Step 3 ── Apply base kustomize overlay (deploy/kind/base/)
     │         Namespaces, HelmRepositories, HelmReleases
     │
Step 4 ── Wait for HelmReleases Ready
     │         cert-manager, openbao, mariadb-operator,
     │         external-secrets, memcached-operator
     │
     ├── Phase 3b: kustomization/rabbitmq-cluster-operator Ready
     │         The RabbitMQ Cluster Operator arrives as a Flux
     │         Kustomization, which the HelmRelease wait above cannot
     │         see. This wait hard-fails: a ControlPlane projects a
     │         RabbitmqCluster for spec.infrastructure.messaging, so
     │         the operator belongs on every cluster this script
     │         provisions.
     │
Step 5 ── Apply infrastructure kustomize overlay (deploy/kind/infrastructure/)
     │         ClusterIssuer, MariaDB CR, Memcached CR,
     │         OpenBao TLS cert, ESO resources
     │         Gated by wait_for_crds on the operator CRDs: memcacheds,
     │         externalsecrets, clustersecretstores, mariadbs,
     │         envoyproxies, the three garage kinds, openbaoclusters,
     │         rabbitmqclusters.rabbitmq.com
     │
Step 6 ── Wait for OpenBao pods Ready
     │
Step 7 ── OpenBao bootstrap
     │         init-unseal → setup-secret-engines →
     │         setup-auth → setup-policies →
     │         write-bootstrap-secrets
     │
Step 8 ── Wait for ExternalSecrets synced
              keystone-admin, keystone-db,
              mariadb-root-password
```

**kind-only ExternalSecret shims.** The `keystone-admin`, `keystone-db`, and
`mariadb-root-password` ExternalSecrets shown above are **kind-overlay shims**
(`deploy/kind/infrastructure/`), not part of the production base. The production
`deploy/eso/` stack ships only the `ClusterSecretStore`: in a ControlPlane-based
deployment the admin password is projected per ControlPlane by the c5c3-operator, and a
non-kind Flux MariaDB baseline provides the `mariadb-root-password` Secret itself.

**Why two-phase kustomize?** The base kustomization contains only built-in Kubernetes
types (Namespaces, HelmRepository, HelmRelease). The infrastructure kustomization
contains CRD-dependent resources (ClusterIssuer, MariaDB CR, Memcached CR) that require
operator CRDs to be installed first. Applying them in two phases prevents
`kubectl apply` failures on fresh clusters where CRDs do not yet exist.

### Idempotent Re-runs

Re-running `make deploy-infra` against an existing cluster converges rather than
failing — each step detects the work it already completed and skips it:

- **Step 1** skips cluster creation when the kind cluster already exists.
- The **containerd nofile cap** skips the write, the containerd restart, and the
  node-Ready wait only when the node's drop-in *and* the limit the running
  containerd reports both already match. Checking the drop-in alone would
  permanently skip a node whose write landed but whose restart failed, leaving
  containerd uncapped behind a clean-looking deploy.
- **Gateway API CRDs** are skipped when all five standard-channel CRDs are
  present (see the sequence above).
- The **kustomize overlays** and **TLS prerequisites** re-apply convergently
  (`kubectl apply` / upserts).
- **OpenBao init, unseal, and bootstrap** detect completed work: initialized
  and unsealed checks, enable-if-missing for engines/auth/policies, and
  write-if-missing for the bootstrap secrets.
- Additional `WITH_*` opt-ins on a re-run install only the newly enabled
  components; the already-deployed ones are left untouched.
- Removing a previously enabled flag does **not** uninstall that component —
  cleanup is `make teardown-infra`'s job.
- `WITH_REGISTRY_CACHE` needs a cluster created with the flag; enabling it on a
  pre-existing cluster leaves the mirrors inert and prints a warning.
- `WITH_CONTROLPLANE` on a provisioned standalone stack is a mode change (the
  ControlPlane owns MariaDB/Memcached), not supported as a re-run — start from a
  fresh cluster.

## Kustomize Overlay Structure

```text
deploy/kind/
├── base/
│   └── kustomization.yaml          References ../../flux-system/
│                                    Patches OpenBao HelmRelease → standalone mode
└── infrastructure/
    └── kustomization.yaml          References ../../flux-system/infrastructure/
                                     Patches MariaDB CR → 1 replica, no Galera
                                     Patches Memcached CR → 1 replica
```

The overlays reference the production FluxCD manifests as their base and apply
strategic merge patches to reduce resource requirements for a single-node kind cluster
(~7GB RAM, 2 vCPUs).

### Base Overlay Patches (OpenBao)

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 (HA) | 1 (standalone) |
| HA enabled | `true` | `false` |
| Raft config | `retry_join` with 3 peers | No `retry_join` (standalone) |
| Storage class | `local-path` | `standard` |

### Infrastructure Overlay Patches

**MariaDB CR (`openstack-db`):**

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 | 1 |
| Galera | enabled | disabled |
| MaxScale | enabled | disabled |
| Storage class | default | `standard` |

**Memcached CR (`openstack-memcached`):**

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 | 1 |

Other operators (cert-manager, mariadb-operator, ESO, memcached-operator) are not
patched — they are single-replica or stateless by default.

## Environment Variables

The deployment script supports configurable timeouts via environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `CLUSTER_NAME` | `cobaltcore` | Kind cluster name |
| `FLUX_OPERATOR_VERSION` | _pinned in script_ | Tag of the flux-operator `install.yaml` release applied in Step 2; kept in sync by Renovate via a `customManager` on `hack/deploy-infra.sh` |
| `HELMRELEASE_TIMEOUT` | `600` | Seconds to wait for HelmReleases Ready (also bounds the `wait_for_fluxinstance` poll in Step 2) |
| `POD_TIMEOUT` | `300` | Seconds to wait for OpenBao pods Ready |
| `EXTERNALSECRET_TIMEOUT` | `120` | Seconds to wait for ExternalSecrets synced |
| `SKIP_KIND_CREATE` | `false` | Skip kind cluster creation (CI mode where cluster is pre-created) |
| `KIND_CONFIG` | `hack/kind-config.yaml` | The kind config `render_kind_config` starts from. Set it to `hack/kind-config-multinode.yaml` (1 control-plane node + 2 workers) for suites that need more than one schedulable node. Both configs bind the same host ports, so two clusters created from them cannot coexist on one host. A custom config must keep its control-plane node at `nodes[0]`, which is the only node the `KIND_HOST_PORT` override rewrites. Read only on the run that creates the cluster: with `SKIP_KIND_CREATE=true` or an existing cluster of that name the value is ignored and the script warns |
| `OPENBAO_NAMESPACE` | `shared-services` | OpenBao namespace (propagated to the bootstrap scripts, which resolve the same variable in `common.sh`). The generic `NAMESPACE` variable is deliberately ignored — chainsaw injects `NAMESPACE=<test namespace>` into e2e script steps |
| `INSTALL_DIR` | `~/.local/bin` | Directory for `install-test-deps.sh` to install tools |
| `WITH_CONTROLPLANE` | `false` | When `true`, the c5c3 `ControlPlane` provisions MariaDB/Memcached in managed mode: deploy-infra skips the shared MariaDB/Memcached CRs and seeds the per-CR OpenBao admin-password paths instead |
| `CONTROLPLANE_OPERATORS` | `flux` | How the ControlPlane operator stack is provided (only when `WITH_CONTROLPLANE=true`). `flux` deploys the published c5c3-operator chart + K-ORC Flux source, un-suspends the keystone-, horizon-, glance-, and placement-operator releases, and pins the self-built operators' `:latest` images to their current digests via `hack/refresh-operator-image-digests.sh` (per-operator image-digest ConfigMaps consumed via `valuesFrom`; re-run with `make refresh-operator-digests` after a merge); `external` suspends the Flux stack and expects the operators to be deployed out of band (as the `e2e-controlplane` CI job does with local dev images + `hack/ci-deploy-korc.sh`) |
| `CONTROLPLANE_NAME` | `controlplane` | Name of the ControlPlane CR under `WITH_CONTROLPLANE=true`; the per-CR OpenBao admin-password bootstrap path derives from it, so it must match the applied CR (the `e2e-controlplane` job sets `controlplane-keystone`) |
| `WITH_OVN_KERNEL_MODULES` | `false` | When `true`, `modprobe` `openvswitch` and `geneve` on the host before the cluster is created, so the OVN chassis suites find the datapath and tunnel modules in the kernel the kind nodes share. Linux only, and it needs root or passwordless sudo: without either the script logs a warning and continues |
| `WITH_REGISTRY_CACHE` | `false` | Local-dev only. When `true`, bring up one distribution-registry (`registry:2`) pull-through proxy per upstream registry (`docker.io`, `ghcr.io`, `registry.k8s.io`, `quay.io`, plus the vanity fronts `oci.external-secrets.io` and `docker-registry3.mariadb.com`) on the `kind` Docker network and wire every node's containerd at them via a `certs.d/<host>/hosts.toml` mirror, so unmodified image refs are served from a persistent local cache that survives `kind delete`. The proxy streams and caches inline (fast even on a cold pull). The containerd mirror patch is injected only into the deploy-time kind config, never the checked-in `hack/kind-config.yaml`, so CI is unaffected. Requires `yq`. See the [Extended Quick Start](../../quick-start-extended.md) |
| `PURGE_REGISTRY_CACHE` | `false` | Consumed by `make teardown-infra`. When `true`, also remove the registry pull-through cache containers and their volumes (identified by the `cobaltcore.registry-cache=true` label). The default leaves them running so the warm cache is reused on the next deploy |

**Example: override HelmRelease timeout:**

```bash
HELMRELEASE_TIMEOUT=600 make deploy-infra
```

## CI Job

The `e2e-infra` job in `.github/workflows/ci.yaml` runs only on pull requests
(`github.event_name == 'pull_request'`) and only when the `e2e-infra` path filter
of the `changes` job matches. It depends only on `changes` — not on the `lint` or
`test` jobs — so it starts as soon as the path filters are resolved.

**Job steps:**

1. Checkout repository (SHA-pinned `actions/checkout`)
2. Setup Go (SHA-pinned `actions/setup-go` with `go-version-file: go.work`)
3. Create kind cluster (SHA-pinned `helm/kind-action` with `hack/kind-config.yaml`)
4. Install Flux CLI (SHA-pinned `fluxcd/flux2/action`)
5. Install test dependencies (`make install-test-deps`, adds `~/.local/bin` to `PATH`)
6. Deploy infrastructure stack (`make deploy-infra` with `SKIP_KIND_CREATE=true`)
7. Run Chainsaw E2E tests against `tests/e2e/infrastructure/`
8. Re-run `make deploy-infra` with unchanged parameters (no `SKIP_KIND_CREATE`, exercises the script's existing-cluster detection)
9. Re-run the full infrastructure suite (report `chainsaw-report-rerun`) to prove the healthy stack is left unchanged
10. Re-run `make deploy-infra` with `WITH_METRICS_SERVER=true` (additive leg — the script's Phase-3 wait gates the new metrics-server HelmRelease on Ready)
11. Run a scoped Chainsaw suite (report `chainsaw-report-additive`) over infra-stack-health, garage-health, flux-web-health, no-prometheus-when-disabled, and openbao-instance, skipping the metrics-server absence suite it would now rightly fail
12. Dump diagnostic info on failure (`kubectl get`, `flux logs` for troubleshooting)
13. Upload JUnit report as workflow artifact (SHA-pinned `actions/upload-artifact`, `if: always()`)

**Configuration:**

| Setting | Value |
| --- | --- |
| `timeout-minutes` | 45 |
| `permissions` | `contents: read` (inherited from workflow-level) |
| `concurrency` | Cancel-in-progress on PRs (inherited from workflow-level) |
| Action pinning | All `uses:` references are SHA-pinned with version comments |

## Chainsaw E2E Test

**File:** `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml`

The test asserts readiness of all deployed components:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | cert-manager Deployment ready | `cert-manager` | `Deployment` |
| 2 | OpenBao StatefulSet ready | `shared-services` | `StatefulSet` |
| 3 | ESO Deployment ready | `external-secrets` | `Deployment` |
| 4 | MariaDB Operator Deployment ready | `mariadb-system` | `Deployment` |
| 5 | Memcached Operator Deployment ready | `memcached-system` | `Deployment` |
| 6 | ClusterIssuer Ready condition | (cluster-scoped) | `ClusterIssuer` |
| 7 | MariaDB CR Ready condition | `openstack` | `MariaDB` |
| 8 | Memcached CR Ready condition | `openstack` | `Memcached` |
| 9 | ClusterSecretStore Valid condition | (cluster-scoped) | `ClusterSecretStore` |
| 10 | ExternalSecrets SecretSynced | `openstack` | `ExternalSecret` (x3) |

Assert timeout is ~5 minutes to account for operator startup time.

The `e2e-infra` job auto-discovers every `chainsaw-test.yaml` under
`tests/e2e/infrastructure/`, so sibling suites run in the same job with no CI wiring.

**File:** `tests/e2e/infrastructure/garage-health/chainsaw-test.yaml`

Covers the Garage object store (the S3 backend for the Glance e2e suites):

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | garage-operator Deployment ready + HelmRelease Ready | `garage-system` | `Deployment`, `HelmRelease` |
| 2 | Credential ExternalSecrets SecretSynced: `garage-admin-token` and `garage-s3-credentials` beside the CRs, plus the retained `garage-s3-credentials` copy the Glance consumers read | `shared-services`, `openstack` | `ExternalSecret` (x3) |
| 3 | GarageCluster `Running`; GarageBucket / GarageKey `Ready` | `shared-services` | `GarageCluster`, `GarageBucket`, `GarageKey` |
| 4 | S3 put + list with the imported key over path-style HTTP; the probe pod stays in `openstack` and reaches Garage through `garage.shared-services.svc.cluster.local` | `openstack` | `script` (throwaway `aws-cli` pod) |

**File:** `tests/e2e/infrastructure/openbao-instance/chainsaw-test.yaml`

Covers the openbao-operator and the proving `OpenBaoCluster` instance:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | openbao-operator Deployment ready + HelmRelease Ready | `openbao-operator-system` | `Deployment`, `HelmRelease` |
| 2 | Unseal-key ExternalSecret SecretSynced, and the instance's `ownerReference` adoption of the Secret it materialized | `openstack` | `ExternalSecret`, `Secret`, `OpenBaoCluster` |
| 3 | OpenBaoCluster `Available` with `readyReplicas: 1`, `APIServerNetworkReady`, and a non-empty `spec.network.apiServerEndpointIPs` | `openstack` | `OpenBaoCluster` |
| 4 | Kubernetes auth to AppRole to KV v2 round-trip, including a rejected login with a wrong secret ID | `openstack` | `script` (`kubectl exec` into the instance pod) |
| 5 | Pod deletion, then the replacement pod reports `sealed: false` without any unseal command | `openstack` | `script` (`kubectl exec` into the instance pod) |

## Pinned Tool Versions

`hack/install-test-deps.sh` installs these pinned versions with SHA256 checksum
verification.  For flux, kind, and kubectl, SHA256 hashes are pinned as constants
in the script (per-platform).  For chainsaw, checksums are fetched from upstream
until pinned hashes are available.  To update hashes after a version bump, download
the new release artifacts, compute `sha256sum`, and replace the values in the script.

| Tool | Version | SHA256 Pinning |
| --- | --- | --- |
| chainsaw | v0.2.15 | upstream (fetched) |
| flux | 2.9.2 | pinned |
| kind | v0.32.0 | pinned |
| kubectl | v1.36.2 | pinned |

## Quick Start

```bash
# Install prerequisites (installs to ~/.local/bin — ensure it is in PATH)
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"

# Deploy infrastructure stack
make deploy-infra

# Run E2E tests
make e2e

# Clean up
make teardown-infra
```

## Related Resources

- [OpenBao Bootstrap Procedure](openbao-bootstrap.md) — OpenBao deployment and bootstrap
- `deploy/flux-system/` — Production FluxCD base manifests
- `deploy/kind/` — Kind-specific kustomize overlays
- `tests/e2e/infrastructure/` — Chainsaw E2E test files
- `.github/workflows/ci.yaml` — CI workflow with `e2e-infra` job
