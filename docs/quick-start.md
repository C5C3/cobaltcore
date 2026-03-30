<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start: Keystone Operator on Kind

This guide walks through running the Keystone operator on a local [kind](https://kind.sigs.k8s.io/)
cluster. It follows the same steps used by the E2E test suite, so the cluster you end up with is
identical to what CI validates.

**What you get after completing this guide:**

- A single-node kind cluster with the full infrastructure stack (cert-manager, MariaDB operator,
  Memcached operator, External Secrets Operator, OpenBao)
- The Keystone operator running with leader election
- A `Keystone` custom resource that provisions a Keystone API service backed by MariaDB and Memcached

---

## Prerequisites

### System requirements

| Resource | Minimum |
|----------|---------|
| RAM | 8 GB available to Docker |
| CPU | 2 cores |
| Disk | 10 GB free |

### Required tools

| Tool | Install |
|------|---------|
| [Docker](https://docs.docker.com/get-docker/) | platform installer |
| [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) | see below |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | see below |
| [Flux CLI](https://fluxcd.io/flux/installation/#install-the-flux-cli) | see below |
| [Helm](https://helm.sh/docs/intro/install/) | platform installer |
| [jq](https://jqlang.org/download/) | platform installer |

The project ships a helper script that downloads and verifies kind, kubectl, and Flux CLI with
pinned SHA256 checksums. The authoritative versions are declared at the top of
`hack/install-test-deps.sh` — always use that script rather than installing these tools manually to
stay in sync with what CI uses:

```bash
make install-test-deps
```

By default, binaries are installed to `~/.local/bin`. Make sure that directory is in your `PATH`:

```bash
export PATH="${HOME}/.local/bin:${PATH}"
```

---

## Step 1 — Clone the repository

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

---

## Step 2 — Create the kind cluster

The project provides a `hack/kind-config.yaml` with a single control-plane node configuration:

```bash
kind create cluster \
  --name forge-e2e \
  --config hack/kind-config.yaml \
  --wait 60s
```

Verify the cluster is ready:

```bash
kubectl cluster-info --context kind-forge-e2e
```

---

## Step 3 — Deploy the infrastructure stack

The `deploy-infra` target runs an 8-step deployment sequence that installs and configures all
dependencies the Keystone operator needs:

```bash
make deploy-infra
```

Internally this performs the following steps:

| Step | What happens |
|------|-------------|
| 1 | Kind cluster already exists — skipped (cluster was created in Step 2) |
| 2 | **Install FluxCD** via `flux install` |
| 3 | Apply base kustomize overlay — namespaces, `HelmRepository` sources, `HelmRelease` objects |
| 4 | Wait for HelmReleases to become `Ready`: cert-manager → OpenBao TLS prerequisites → prometheus-operator-crds, openbao, mariadb-operator, external-secrets, memcached-operator |
| 5 | Apply infrastructure kustomize overlay — `ClusterSecretStore`, `ExternalSecret` objects, `MariaDB` and `Memcached` cluster CRs |
| 6 | Wait for the OpenBao pod to reach `Running` phase |
| 7 | **Bootstrap OpenBao** — initialize, unseal (5 shares, 3-of-5 threshold), configure secret engines, auth methods, policies, and seed the bootstrap secrets |
| 8 | Wait for `ExternalSecret` objects (`keystone-admin`, `keystone-db`, `mariadb-root-password`) to sync their Kubernetes `Secret` counterparts from OpenBao |

The script also triggers a re-reconciliation of the `openstack-db` MariaDB CR and waits for it to
become `Ready` before returning.

Expected duration: **5–10 minutes** on first run (image pulls dominate).

::: tip Configurable timeouts
Override the default timeouts via environment variables if your machine is slow:

```bash
HELMRELEASE_TIMEOUT=900 POD_TIMEOUT=600 make deploy-infra
```
:::

::: details What gets deployed
After `make deploy-infra` completes the following are running in the cluster:

```
cert-manager          cert-manager-*         Ready
mariadb-system        mariadb-operator-*     Ready
memcached-system      memcached-operator-*   Ready
external-secrets      external-secrets-*     Ready
openbao-system        openbao-0              Ready (unsealed)
openstack             openstack-db-*         Ready (MariaDB cluster)
openstack             openstack-memcached-*  Ready (Memcached cluster)
```

The `openstack` namespace also holds the synced secrets:

```
openstack   keystone-admin        (admin password from OpenBao)
openstack   keystone-db           (database credentials from OpenBao)
```
:::

---

## Step 4 — Deploy the Keystone operator

The Keystone operator is distributed as a Helm chart. There are two ways to deploy it depending on
your goal.

### Option A — From GHCR (recommended for users)

Install the published chart directly from the GHCR OCI registry. Flux reconciles updates
automatically:

```bash
# Add the HelmRepository source (already present if you ran make deploy-infra)
kubectl apply -f deploy/flux-system/sources/c5c3-charts.yaml

# Apply the HelmRelease — Flux installs the chart and keeps it reconciled
kubectl apply -f deploy/flux-system/releases/keystone-operator.yaml

# Wait until the operator is Ready
kubectl wait helmrelease/keystone-operator \
  -n openstack \
  --for=condition=Ready \
  --timeout=120s
```

The `HelmRelease` references chart version `>=0.1.0 <1.0.0` from the `c5c3-charts` repository and
deploys 2 replicas with leader election enabled.

### Option B — From the local Helm chart (recommended for development)

Build the operator image, load it into kind, and install the chart directly from the repository:

```bash
# 1. Build the operator image
make docker-build OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev

# 2. Load the image into the kind cluster (no registry needed)
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name forge-e2e

# 3. Pre-install CRDs so the API server watch cache is ready before the
#    operator starts (avoids missing initial watch events)
kubectl apply -f operators/keystone/helm/keystone-operator/crds/
kubectl wait crd --all --for condition=Established --timeout=60s

# 4. Install the chart
helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait --timeout 120s
```

### Verify the operator is running

```bash
kubectl get pods -n openstack -l app.kubernetes.io/name=keystone-operator
```

Expected output:

```
NAME                                  READY   STATUS    RESTARTS   AGE
keystone-operator-6d7f9f4d5b-abc12   1/1     Running   0          30s
keystone-operator-6d7f9f4d5b-xyz99   1/1     Running   0          30s
```

---

## Step 5 — Build and load the Keystone service image

The `Keystone` CR references a service image that runs the actual OpenStack Keystone API. Either
pull the pre-built image from GHCR or build it locally.

Set the release you want to work with. The default is the most recent release; update this variable
whenever a new release is available:

```bash
RELEASE=2025.2   # update to the target release
```

### Pull from GHCR

```bash
docker pull ghcr.io/c5c3/keystone:"${RELEASE}"
kind load docker-image ghcr.io/c5c3/keystone:"${RELEASE}" --name forge-e2e
```

### Build locally

```bash
# Resolve the upstream source ref
SERVICE_REF=$(yq '.keystone' "releases/${RELEASE}/source-refs.yaml")
PIP_EXTRAS=$(yq -r '.keystone.pip_extras // [] | join(",")' "releases/${RELEASE}/extra-packages.yaml")
PIP_PACKAGES=$(yq -r '.keystone.pip_packages // [] | join(" ")' "releases/${RELEASE}/extra-packages.yaml")
APT_PACKAGES=$(yq -r '.keystone.apt_packages // [] | join(" ")' "releases/${RELEASE}/extra-packages.yaml")

# Clone upstream Keystone at the pinned ref
git clone --depth 1 --branch "${SERVICE_REF}" \
  https://github.com/openstack/keystone.git /tmp/keystone-src

# Apply constraint overrides
scripts/apply-constraint-overrides.sh "${RELEASE}"

# Build the image chain
docker build -t python-base images/python-base/
docker build -t venv-builder images/venv-builder/
docker build -t "ghcr.io/c5c3/keystone:${RELEASE}" \
  --build-arg PIP_EXTRAS="${PIP_EXTRAS}" \
  --build-arg PIP_PACKAGES="${PIP_PACKAGES}" \
  --build-arg EXTRA_APT_PACKAGES="${APT_PACKAGES}" \
  --build-context keystone=/tmp/keystone-src \
  --build-context "upper-constraints=releases/${RELEASE}" \
  images/keystone/

# Load into kind
kind load docker-image "ghcr.io/c5c3/keystone:${RELEASE}" --name forge-e2e
```

---

## Step 6 — Create a Keystone CR

Apply the following manifest to deploy a Keystone instance in **managed mode**. In this mode the
operator creates and manages the MariaDB database (via `clusterRef`) and configures Memcached
for session caching. Replace `<RELEASE>` with the same value used in Step 5 (e.g. `2025.2`):

```yaml
# keystone.yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  replicas: 3
  image:
    repository: ghcr.io/c5c3/keystone
    tag: "<RELEASE>"   # e.g. 2025.2 — must match the image loaded in Step 5
  database:
    clusterRef:
      name: openstack-db
    database: keystone
    secretRef:
      name: keystone-db
  cache:
    clusterRef:
      name: openstack-memcached
    backend: dogpile.cache.pymemcache
  fernet:
    rotationSchedule: "0 0 * * 0"   # weekly rotation, every Sunday at midnight
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
```

```bash
kubectl apply -f keystone.yaml
```

---

## Step 7 — Wait for Keystone to become Ready

The operator reconciles the CR through five sequential sub-conditions before the aggregate
`Ready` condition is set:

| Condition | What it waits for |
|-----------|-------------------|
| `SecretsReady` | `keystone-db` and `keystone-admin` Secrets are available |
| `FernetKeysReady` | Fernet key Secret and CronJob created |
| `DatabaseReady` | `db_sync` Job completed successfully |
| `DeploymentReady` | Keystone API Deployment has available replicas |
| `BootstrapReady` | Bootstrap Job completed (admin user, region, endpoints) |

Watch the conditions with:

```bash
kubectl get keystone keystone -n openstack -w
```

Or wait for `Ready=True`:

```bash
kubectl wait keystone/keystone \
  -n openstack \
  --for=condition=Ready \
  --timeout=5m
```

Expected output (once reconciled):

```
keystone.keystone.openstack.c5c3.io/keystone condition met
```

---

## Step 8 — Verify the deployment

### Check all owned resources

```bash
# Deployment and Service
kubectl get deployment,service -n openstack -l app.kubernetes.io/instance=keystone

# Fernet rotation CronJob and Secret
kubectl get cronjob,secret -n openstack -l app.kubernetes.io/instance=keystone

# ConfigMap (name includes a content-hash suffix)
kubectl get cm -n openstack | grep keystone-config-
```

### Query the Keystone API

Run a one-shot pod inside the cluster to reach the API service:

```bash
kubectl run curl-test \
  --image="ghcr.io/c5c3/keystone:${RELEASE}" \
  --rm -i --restart=Never \
  --command -- python3 -c \
  "import urllib.request; print(urllib.request.urlopen('http://keystone-api.openstack.svc:5000/v3').read().decode())"
```

A successful response returns a JSON document beginning with `{"version": {"id": "v3", ...}}`.

### Inspect conditions in detail

```bash
kubectl get keystone keystone -n openstack -o jsonpath='{.status.conditions}' | jq .
```

---

## Access Keystone from your local machine

The Keystone Service is of type `ClusterIP` and is only reachable inside the cluster. To access the
API from your workstation, forward the port with `kubectl` and export the OpenStack credentials from
the `keystone-admin` Secret that was synced from OpenBao during `make deploy-infra`.

### Step 1 — Forward port 5000

```bash
kubectl port-forward svc/keystone-api -n openstack 5000:5000
```

Leave this running in a separate terminal. The API is now available at `http://localhost:5000`.

### Step 2 — Export OpenStack credentials

In a second terminal, read the admin password directly from the cluster and set the standard
OpenStack environment variables:

```bash
export OS_AUTH_URL=http://localhost:5000/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
```

### Step 3 — Verify access

Check that the API responds and that authentication works:

```bash
# Unauthenticated version endpoint
curl http://localhost:5000/v3

# Authenticated token request (requires python-openstackclient)
openstack token issue
```

A successful `curl` response begins with `{"version": {"id": "v3", ...}}`. A successful
`openstack token issue` prints a table with the issued token and its expiry.

> **Note:** The service catalog returned by Keystone contains cluster-internal endpoint URLs
> (e.g. `http://keystone-api.openstack.svc.cluster.local:5000/v3`). Only `identity` commands
> that authenticate directly against `OS_AUTH_URL` work via port-forward. Commands that resolve
> other service endpoints from the catalog (Nova, Neutron, …) require additional port-forwards
> for each service.

---

## Running the E2E test suite

The project ships a full [Chainsaw](https://kyverno.github.io/chainsaw/) test suite that validates
all operator behaviour. With the cluster running from the steps above, execute:

```bash
# All Keystone test suites (10 suites, up to 4 in parallel)
make e2e

# Or target a specific suite
chainsaw test \
  --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/keystone/basic-deployment/
```

Available test suites under `tests/e2e/keystone/`:

| Suite | What it validates |
|-------|-------------------|
| `basic-deployment` | Full happy-path reconciliation, API accessibility |
| `missing-secret` | Recovery when a referenced Secret is absent |
| `fernet-rotation` | Manual key rotation triggers Deployment rolling restart |
| `scale` | Replicas scale from 3 → 5 → 2 correctly |
| `deletion-cleanup` | All owned resources are garbage-collected on CR deletion |
| `policy-overrides` | `oslo.policy` ConfigMap integration |
| `middleware-config` | WSGI `api-paste.ini` middleware pipeline customization |
| `brownfield-database` | Explicit database host (no MariaDB CR created) |
| `image-upgrade` | Rolling image update with zero downtime |
| `invalid-cr` | Webhook rejects invalid cron expressions and duplicate plugins |

JUnit XML reports are written to `_output/reports/` after each run.

---

## Teardown

Delete the kind cluster and all its resources:

```bash
make teardown-infra
```

This runs `kind delete cluster --name forge-e2e` and removes all local state.
