---
title: E2E Infrastructure Deployment
quadrant: infrastructure
feature: CC-0010
---

# E2E Infrastructure Deployment

Reference documentation for the end-to-end infrastructure deployment automation (CC-0010).
This feature provides Makefile targets, shell scripts, kustomize overlays, and a CI job that
deploy the full infrastructure stack (cert-manager, OpenBao, ESO, MariaDB Operator,
Memcached Operator, infrastructure CRs, ExternalSecrets) into a local kind cluster and
validate it with Chainsaw E2E tests.

## Prerequisites

| Prerequisite | Minimum Version | Purpose |
| --- | --- | --- |
| Docker | 20.10+ | Container runtime for kind nodes |
| kubectl | v1.32+ | Kubernetes CLI for cluster interaction |
| kind | v0.27+ | Local Kubernetes cluster provisioning |
| flux | 2.5+ | FluxCD CLI for installing Flux components |
| chainsaw | v0.3+ | Kyverno Chainsaw E2E test runner |
| jq | 1.6+ | JSON processing in wait functions |
| kustomize | 5.0+ | Manifest rendering (bundled with kubectl) |

`make install-test-deps` installs pinned versions of chainsaw, flux, kind, and kubectl
automatically. Docker and jq must be installed separately.

## Makefile Targets

Four targets orchestrate the E2E workflow:

| Target | Command | Description |
| --- | --- | --- |
| `deploy-infra` | `hack/deploy-infra.sh` | Deploy the full infrastructure stack to a kind cluster |
| `teardown-infra` | `hack/teardown-infra.sh` | Delete the kind cluster and free resources |
| `install-test-deps` | `hack/install-test-deps.sh` | Install pinned E2E tool versions |
| `e2e` | `chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/` | Run all Chainsaw E2E tests |

### Typical Workflow

```bash
# 1. Install E2E tool dependencies (idempotent)
make install-test-deps

# 2. Deploy the infrastructure stack
make deploy-infra

# 3. Run E2E tests
make e2e

# 4. Tear down the cluster when done
make teardown-infra
```

## Deployment Sequence

`make deploy-infra` executes an 8-step deployment sequence. Each step depends on
the successful completion of the previous step.

```text
Step 1: kind create cluster
  │     Creates a single control-plane kind cluster using hack/kind-config.yaml.
  ▼
Step 2: flux install
  │     Installs FluxCD source-controller and helm-controller into flux-system namespace.
  ▼
Step 3: kubectl apply -k deploy/kind/base
  │     Applies namespaces, HelmRepository sources, and HelmRelease operators
  │     (with kind-specific OpenBao patches for single-node Raft mode).
  ▼
Step 4: Wait for HelmReleases Ready
  │     Polls all HelmReleases across all namespaces until every one has
  │     condition Ready=True, or times out.
  │     Expected HelmReleases: cert-manager, openbao, mariadb-operator,
  │     external-secrets, memcached-operator.
  ▼
Step 5: kubectl apply -k deploy/kind/infrastructure
  │     Applies CRD-dependent infrastructure resources (ClusterIssuer,
  │     MariaDB CR, Memcached CR) with kind-specific patches.
  ▼
Step 6: Wait for OpenBao pods Ready
  │     Waits for pods matching label app.kubernetes.io/name=openbao in
  │     openbao-system namespace to reach Ready condition.
  ▼
Step 7: OpenBao bootstrap
  │     Runs all 5 bootstrap scripts in sequence:
  │       7a. init-unseal.sh      — Initialize Shamir keys, unseal the single replica
  │       7b. setup-secret-engines.sh — Enable KV v2 and PKI engines
  │       7c. setup-auth.sh       — Configure Kubernetes and AppRole auth
  │       7d. setup-policies.sh   — Apply HCL access control policies
  │       7e. write-bootstrap-secrets.sh — Generate and seed initial credentials
  │     The root token is extracted from the openbao-init-keys Secret and
  │     exported as BAO_TOKEN for scripts 7b–7e.
  ▼
Step 8: Wait for ExternalSecrets synced
        Polls ExternalSecrets in the openstack namespace until all reach
        Ready=True condition (SecretSynced), confirming end-to-end secret
        delivery from OpenBao through the ClusterSecretStore.
        Expected ExternalSecrets: keystone-admin, keystone-db, mariadb-root-password.
```

### Pre-flight Checks

Before creating the cluster, `deploy-infra.sh` verifies:

1. **Docker daemon is running** — exits with error if `docker info` fails.
2. **No existing cluster** — if a kind cluster with the same name already exists,
   cluster creation is skipped and deployment continues from step 2. This allows
   re-running `make deploy-infra` against an existing cluster.

## Environment Variables

All timeouts and the cluster name are configurable via environment variables.
Defaults are chosen to work on developer laptops and CI runners.

| Variable | Default | Description |
| --- | --- | --- |
| `CLUSTER_NAME` | `c5c3` | Name of the kind cluster |
| `HELM_RELEASE_TIMEOUT` | `600s` | Maximum wait time for all HelmReleases to become Ready |
| `OPENBAO_POD_TIMEOUT` | `300s` | Maximum wait time for OpenBao pods to become Ready |
| `EXTERNAL_SECRET_TIMEOUT` | `300s` | Maximum wait time for ExternalSecrets to sync |
| `INSTALL_DIR` | `$HOME/.local/bin` | Directory where `install-test-deps.sh` installs binaries |

### Overriding Timeouts

```bash
# Increase HelmRelease timeout for slow networks
HELM_RELEASE_TIMEOUT=900s make deploy-infra

# Use a custom cluster name
CLUSTER_NAME=my-cluster make deploy-infra
CLUSTER_NAME=my-cluster make teardown-infra
```

## Kustomize Overlay Structure

The `deploy/kind/` directory contains kustomize overlays that patch production manifests
for single-node kind clusters. This ensures kind testing validates the same manifests
that ship to production, with only resource sizing differences.

```text
deploy/
├── kind/
│   ├── base/
│   │   └── kustomization.yaml          Patches OpenBao for single-node Raft mode
│   └── infrastructure/
│       └── kustomization.yaml          Patches MariaDB and Memcached for single replica
└── flux-system/                        (production base — referenced by kind overlays)
    ├── kustomization.yaml              Base resources (namespaces, sources, releases)
    └── infrastructure/
        └── kustomization.yaml          CRD-dependent resources (CRs)
```

### Base Overlay (`deploy/kind/base/`)

**Base reference:** `../../flux-system/` (production manifests)

Patches the OpenBao HelmRelease for single-node Raft mode:

| Setting | Production | Kind Overlay |
| --- | --- | --- |
| `server.ha.enabled` | `true` | `true` |
| `server.ha.replicas` | `3` | `1` |
| `server.ha.raft.config` | Includes `retry_join` peers | Single-node (no `retry_join`) |
| `dataStorage.storageClass` | `local-path` | `standard` |

HA mode remains enabled (`ha.enabled: true`) so the Helm chart renders the
`ha.raft.config` stanza. With `ha.enabled: false`, the chart silently falls back
to `standalone.config`, ignoring any custom Raft configuration. The kind overlay
omits `retry_join` stanzas — a single Raft node bootstraps automatically without
peer discovery.

### Infrastructure Overlay (`deploy/kind/infrastructure/`)

**Base reference:** `../../flux-system/infrastructure/` (production CRs)

Patches MariaDB and Memcached CRs for single-replica mode:

| Resource | Setting | Production | Kind Overlay |
| --- | --- | --- | --- |
| MariaDB `openstack-db` | `replicas` | `3` | `1` |
| MariaDB `openstack-db` | `galera.enabled` | `true` | `false` |
| MariaDB `openstack-db` | `maxScale.enabled` | `true` | `false` |
| MariaDB `openstack-db` | `storage.storageClassName` | `ceph-rbd` | `standard` |
| Memcached `openstack-memcached` | `replicas` | `3` | `1` |

Other operators (cert-manager, mariadb-operator, ESO, memcached-operator) are not
patched — they are single-replica or stateless by default and run correctly on
a single-node kind cluster without modification.

### Validating Overlays

```bash
# Render base overlay (should show OpenBao with ha.enabled: true, replicas: 1)
kustomize build deploy/kind/base/

# Render infrastructure overlay (should show MariaDB replicas: 1)
kustomize build deploy/kind/infrastructure/
```

## Kind Cluster Configuration

**File:** `hack/kind-config.yaml`

The kind cluster uses a single control-plane node with ingress port mappings,
suitable for developer laptops and CI runners (~7 GB RAM, 2 vCPUs).

| Setting | Value |
| --- | --- |
| API version | `kind.x-k8s.io/v1alpha4` |
| Nodes | 1 control-plane (no workers) |
| Ingress label | `ingress-ready=true` |
| Port 80 | Mapped to host (HTTP) |
| Port 443 | Mapped to host (HTTPS) |

## Chainsaw E2E Test

**File:** `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml`

The `infra-stack-health` test asserts readiness of all deployed components across
4 steps. It uses `apiVersion: chainsaw.kyverno.io/v1alpha2` and a 5-minute assert
timeout to account for operator startup time.

### Step 1: Operator Deployments Ready

Asserts `availableReplicas > 0` on:

| Deployment | Namespace |
| --- | --- |
| `cert-manager` | `cert-manager` |
| `external-secrets` | `external-secrets` |
| `mariadb-operator` | `mariadb-system` |
| `memcached-operator-controller-manager` | `memcached-system` |

### Step 2: OpenBao StatefulSet Ready

Asserts `readyReplicas >= 1` on the `openbao` StatefulSet in `openbao-system`.

### Step 3: Infrastructure CRs Ready

Asserts `Ready` condition with `status: "True"` on:

| Resource | Kind | Namespace |
| --- | --- | --- |
| `selfsigned-cluster-issuer` | `ClusterIssuer` | (cluster-scoped) |
| `openstack-db` | `MariaDB` | `openstack` |
| `openstack-memcached` | `Memcached` | `openstack` |

### Step 4: ESO Resources Functional

Asserts condition status `"True"` on:

| Resource | Kind | Condition | Namespace |
| --- | --- | --- | --- |
| `openbao-cluster-store` | `ClusterSecretStore` | `Valid` | (cluster-scoped) |
| `keystone-admin` | `ExternalSecret` | `SecretSynced` | `openstack` |
| `keystone-db` | `ExternalSecret` | `SecretSynced` | `openstack` |
| `mariadb-root-password` | `ExternalSecret` | `SecretSynced` | `openstack` |

### Test Configuration

The Chainsaw configuration at `tests/e2e/chainsaw-config.yaml` applies to all E2E tests:

| Setting | Value | Purpose |
| --- | --- | --- |
| `execution.failFast` | `true` | Stop on first failure |
| `execution.parallel` | `4` | Run independent tests in parallel |
| `report.format` | `JUNIT-TEST` | JUnit XML output for CI |
| `report.path` | `_output/reports` | Report output directory |

## Script Reference

### deploy-infra.sh

**File:** `hack/deploy-infra.sh`

Orchestrates the full 8-step deployment sequence. Uses `set -euo pipefail`
for strict error handling. All log messages are ISO 8601 UTC timestamped.

**Helper functions:**

| Function | Purpose |
| --- | --- |
| `log()` | Print timestamped log message |
| `wait_for_helmreleases()` | Poll HelmReleases until all Ready or timeout |
| `wait_for_pods()` | Poll for pod existence, then wait for Ready condition (see note below) |
| `wait_for_externalsecrets()` | Poll ExternalSecrets until all synced or timeout |
| `preflight()` | Verify Docker is running and check for existing cluster |

**`wait_for_pods` timeout behavior:** The function first polls until at least one pod
matching the label selector exists, then hands off to `kubectl wait --for=condition=Ready`.
Both phases use the full configured timeout independently, so the effective maximum wait
can be up to 2x the configured value (e.g., 600s for a 300s timeout). This is intentional —
the CI job timeout (40 min) provides the outer bound, while script-level timeouts produce
diagnostics before the runner is killed.

**Exit codes:**

| Code | Meaning |
| --- | --- |
| `0` | Deployment completed successfully |
| `1` | Pre-flight check failed (Docker not running) or timeout exceeded |
| Non-zero | Any step failure (propagated by `set -e`) |

### teardown-infra.sh

**File:** `hack/teardown-infra.sh`

Deletes the kind cluster by name. Idempotent — exits 0 if the cluster does not exist.

```bash
# Delete the default cluster
make teardown-infra

# Delete a custom-named cluster
CLUSTER_NAME=my-cluster hack/teardown-infra.sh
```

### install-test-deps.sh

**File:** `hack/install-test-deps.sh`

Installs pinned versions of E2E test dependencies. Idempotent — skips download
if the tool already exists at the correct version.

**Pinned versions:**

| Tool | Version |
| --- | --- |
| chainsaw | v0.2.14 |
| flux | 2.5.1 |
| kind | v0.27.0 |
| kubectl | v1.32.3 |

**Install directory:** `$HOME/.local/bin` (override with `INSTALL_DIR`).

The script detects OS and architecture automatically (`linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`). Each tool is downloaded as either a tarball or
binary depending on its release format.

## CI Job: e2e-infra

**File:** `.github/workflows/ci.yaml` (job `e2e-infra`)

The `e2e-infra` job runs the full infrastructure deployment and E2E tests on every
push to `main` and on every pull request. It runs independently of the `lint` and
`test` jobs (no `needs:` dependency).

### Job Configuration

| Setting | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `40` |
| `permissions` | `contents: read` |

### Steps

| # | Step | Action |
| --- | --- | --- |
| 1 | Checkout | `actions/checkout` (SHA-pinned) |
| 2 | Setup Go | `actions/setup-go` (SHA-pinned, `go-version-file: go.work`) |
| 3 | Create kind cluster | `helm/kind-action` (SHA-pinned, `config: hack/kind-config.yaml`, `cluster_name: c5c3`) |
| 4 | Install test dependencies | `make install-test-deps` + add `~/.local/bin` to `GITHUB_PATH` |
| 5 | Deploy infrastructure | `make deploy-infra` |
| 6 | Run E2E tests | `chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/infrastructure/` |
| 7 | Upload test report | `actions/upload-artifact` (SHA-pinned, `if: always()`, path `_output/reports/`) |

All `uses:` references are SHA-pinned with trailing version comments (e.g.,
`actions/checkout@de0fac2e...  # v6`) per repository convention to prevent
supply chain attacks via mutable tag retargeting.

### Concurrency

The job inherits the workflow-level concurrency group
`${{ github.ref }}-${{ github.workflow }}` with `cancel-in-progress` on pull
requests. Push-to-main runs are never cancelled.

## Troubleshooting

### HelmRelease timeout

If `wait_for_helmreleases` times out, check which releases are not ready:

```bash
kubectl get helmreleases --all-namespaces
kubectl describe helmrelease <name> -n <namespace>
```

Common causes:
- Slow image pulls (increase `HELM_RELEASE_TIMEOUT`)
- Chart version constraint not satisfied (check HelmRepository sync status)
- Webhook certificate not ready (cert-manager must be healthy first)

### OpenBao bootstrap failure

If the bootstrap scripts fail:

```bash
# Check OpenBao pod status
kubectl get pods -n openbao-system

# Check OpenBao logs
kubectl logs openbao-0 -n openbao-system

# Verify TLS certificate exists
kubectl get secret openbao-tls -n openbao-system
```

See [OpenBao Bootstrap Procedure](openbao-bootstrap.md) for detailed bootstrap
script documentation and troubleshooting.

### ExternalSecrets not syncing

```bash
# Check ClusterSecretStore status
kubectl get clustersecretstore openbao-cluster-store -o yaml

# Check ExternalSecret status
kubectl get externalsecrets -n openstack -o wide

# Check ESO controller logs
kubectl logs -n external-secrets -l app.kubernetes.io/name=external-secrets
```

Common causes:
- OpenBao is sealed (re-run `deploy/openbao/bootstrap/init-unseal.sh`)
- Kubernetes auth not configured (re-run bootstrap from step 7c)
- Policy does not cover the requested secret path

### Teardown fails

If `make teardown-infra` fails, force-delete the kind cluster:

```bash
kind delete cluster --name c5c3
# If Docker containers remain:
docker rm -f $(docker ps -q --filter "label=io.x-k8s.kind.cluster=c5c3")
```

## Related Resources

- [Infrastructure Manifests](../infrastructure-manifests.md) -- FluxCD base deployment (CC-0008)
- [OpenBao Bootstrap Procedure](openbao-bootstrap.md) -- Bootstrap scripts reference (CC-0009)
- `hack/kind-config.yaml` -- Kind cluster configuration
- `deploy/kind/` -- Kustomize overlays for kind
- `tests/e2e/chainsaw-config.yaml` -- Chainsaw test configuration
- `.github/workflows/ci.yaml` -- CI workflow with e2e-infra job
