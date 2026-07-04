---
title: Quick Start (Extended)
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start (Extended): Keystone Operator on Kind

> Looking for the shortest happy-path on macOS with port `8443`?
> See the compact [Quick Start](./quick-start.md). This page is the
> deep-dive variant with UI tours, fallbacks, the local-build path,
> the production HelmRelease, E2E and Tempest.

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

### Required and optional tools

| Tool | Install |
|------|---------|
| [Docker](https://docs.docker.com/get-docker/) | platform installer |
| [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) | see below |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | see below |
| [Helm](https://helm.sh/docs/intro/install/) | platform installer |
| [jq](https://jqlang.org/download/) | platform installer |
| [Flux CLI](https://fluxcd.io/flux/installation/) | **Optional — debugging only.** `make deploy-infra` bootstraps Flux via flux-operator + `FluxInstance` and does not require the CLI. Install it only to run ad-hoc `flux logs` or `flux get` commands against a live cluster. |

The project ships a helper script that downloads and verifies kind and kubectl with pinned
SHA256 checksums. The authoritative versions are declared at the top of
`hack/install-test-deps.sh` — always use that script rather than installing these tools manually to
stay in sync with what CI uses:

```bash
make install-test-deps
```

To additionally install the optional Flux CLI (for diagnostics only), opt in explicitly:

```bash
WITH_FLUX_CLI=true make install-test-deps
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
  --name forge \
  --config hack/kind-config.yaml \
  --wait 60s
```

Verify the cluster is ready:

```bash
kubectl cluster-info --context kind-forge
```

::: warning Host port 443 binding requirement
The kind config binds host TCP port `443` (mapped to NodePort `31443`) so the
[Quick Start endpoint](#access-keystone-from-your-local-machine)
`https://keystone.127-0-0-1.nip.io/v3` resolves directly from your workstation.
Binding a port below 1024 is privileged on every host OS, and the failure mode
differs by platform.

**Linux** requires `CAP_NET_BIND_SERVICE`, granted by default with **rootful
Docker**. With **rootless Docker** or **Podman**, or when the host sets
`net.ipv4.ip_unprivileged_port_start` to `443` or higher, `kind create
cluster` fails with `failed to start container … bind: permission denied`
on port 443.

**macOS** Docker Desktop binds privileged ports through a system helper
(`vmnetd`) at `/var/run/com.docker.vmnetd.sock`. If that socket is missing —
common after a user-mode install of Docker Desktop, or when you are using
Colima / OrbStack / Rancher Desktop — `kind create cluster` fails with
`connecting to /var/run/com.docker.vmnetd.sock: dial unix … no such file or
directory`. Fix it by enabling **Docker Desktop → Settings → Advanced →
"Allow privileged port mapping"** (Docker prompts for an admin password and
installs the helper), or run
`sudo /Applications/Docker.app/Contents/MacOS/install vmnetd` and restart
Docker Desktop. Other Docker runtimes do not ship `vmnetd` — use the override
below instead.

**Override the host port without privileged binding (any OS):** export
`KIND_HOST_PORT` before `make deploy-infra` and the script renders a
non-privileged kind config on the fly. The Envoy proxy still listens on
NodePort `31443` inside the cluster — only the host-side bind moves.

```bash
export KIND_HOST_PORT=8443
make deploy-infra
# Endpoint becomes https://keystone.127-0-0-1.nip.io:8443/v3 — your
# Keystone CR's `spec.bootstrap.publicEndpoint` must include the same
# `:8443` suffix so the issued service catalog matches the reachable URL.
# See Step 7 for a ready-to-apply CR example.
```

The `KIND_HOST_PORT` override needs `yq` on PATH (only required when the value
differs from `443`). The Chainsaw E2E suites under
`tests/e2e/keystone/gateway-quick-start*` hard-code the default
`https://keystone.127-0-0-1.nip.io/v3` URL and **will not pass with an
override** — use the
[`kubectl port-forward` fallback](#fallback-kubectl-port-forward) for those
suites or run them in CI (Linux + rootful Docker).
:::

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
| 2 | **Install flux-operator** + apply `FluxInstance/flux` — flux-operator reconciles the Flux controller Deployments from the `FluxInstance` spec, then the step blocks until `FluxInstance/flux` reports `Ready=True`. Reaching `Ready=True` guarantees the Flux toolkit CRDs (`source.toolkit.fluxcd.io`, `helm.toolkit.fluxcd.io`, `kustomize.toolkit.fluxcd.io`, `notification.toolkit.fluxcd.io`) are registered, so Step 3 can apply `HelmRepository` and `HelmRelease` objects without a separate `wait_for_crds` gate |
| 2a | **Install Gateway API CRDs** — `kubectl apply --server-side` of the upstream `standard-install.yaml` for the version in `GATEWAY_API_VERSION` (default matches `sigs.k8s.io/gateway-api` in `operators/keystone/go.mod`). Required by the keystone-operator's HTTPRoute watch — without it the operator logs `no matches for kind HTTPRoute` at startup. |
| 2b | **Install Envoy Gateway + `openstack-gw` Gateway** (kind-only) — base overlay installs the `envoy-gateway` HelmRelease and creates `GatewayClass/envoy`, `Certificate/keystone-nip-io-tls`, and `Gateway/openstack-gw` so `https://keystone.127-0-0-1.nip.io/v3` becomes reachable from the developer's host once Keystone attaches an HTTPRoute in Step 7. Production overlays exclude this. |
| 3 | Apply base kustomize overlay — namespaces, `HelmRepository` sources, `HelmRelease` objects (the Flux toolkit CRDs they depend on were registered by Step 2) |
| 4 | Wait for HelmReleases to become `Ready`: cert-manager → OpenBao TLS prerequisites → prometheus-operator-crds, openbao, mariadb-operator, external-secrets, memcached-operator |
| 5 | Apply infrastructure kustomize overlay — `ClusterSecretStore`, `ExternalSecret` objects, `MariaDB` and `Memcached` cluster CRs |
| 6 | Wait for the OpenBao pod to reach `Running` phase |
| 7 | **Bootstrap OpenBao** — initialize, unseal (5 shares, 3-of-5 threshold), configure secret engines, auth methods, policies, and seed the bootstrap secrets |
| 8 | Wait for `ExternalSecret` objects (`keystone-admin`, `keystone-db`, `mariadb-root-password`) to sync their Kubernetes `Secret` counterparts from OpenBao |

The script also triggers a re-reconciliation of the `openstack-db` MariaDB CR and waits for it to
become `Ready` before returning.

::: tip kind-only ExternalSecret shims
The `keystone-admin`, `keystone-db`, and `mariadb-root-password` ExternalSecrets applied
in Step 5 and awaited in Step 8 are **kind-overlay shims**
(`deploy/kind/infrastructure/`) that keep this standalone Keystone flow self-contained.
The production stack (`deploy/eso/`) ships only the `ClusterSecretStore`: in a
ControlPlane-based deployment the admin password is projected per ControlPlane by the
c5c3-operator, and the MariaDB root password is expected from a non-kind Flux MariaDB
baseline.
:::

Expected duration: **5–10 minutes** on first run (image pulls dominate).

::: tip Configurable timeouts
Override the default timeouts via environment variables if your machine is slow:

```bash
HELMRELEASE_TIMEOUT=900 POD_TIMEOUT=600 make deploy-infra
```
:::

::: tip Inspect the Flux installation
flux-operator publishes a `FluxReport/flux` that summarizes the state of every Flux
controller, its version, and the entitlement status. Inspect it at any time:

```bash
kubectl get fluxreport/flux -n flux-system -o yaml
```
:::

::: details What gets deployed
After `make deploy-infra` completes the following are running in the cluster:

```
flux-system           flux-operator-*                Ready (reconciles FluxInstance/flux)
flux-system           source-controller-*            Ready
flux-system           kustomize-controller-*         Ready
flux-system           helm-controller-*              Ready
flux-system           notification-controller-*      Ready
flux-system           flux-web-*                     Ready (kind-only; see Step 4a)
envoy-gateway-system  envoy-gateway-*                Ready (kind-only; provides openstack-gw — see Step 2b)
cert-manager          cert-manager-*         Ready
mariadb-system        mariadb-operator-*     Ready
memcached-system      memcached-operator-*   Ready
external-secrets      external-secrets-*     Ready
openbao-system        openbao-0              Ready (unsealed; kind overlay enables the UI on :8200 — see Step 4b)
openstack             openstack-db-*         Ready (MariaDB cluster)
openstack             openstack-memcached-*  Ready (Memcached cluster)
headlamp-system       headlamp-*             Starting (kind-only demo UI, not waited on)
monitoring            kube-prometheus-stack-prometheus-*   Ready (kind-only; WITH_PROMETHEUS=true; see Step 4c)
monitoring            kube-prometheus-stack-grafana-*      Ready (kind-only; WITH_PROMETHEUS=true; see Step 4c)
monitoring            kube-prometheus-stack-operator-*     Ready (kind-only; WITH_PROMETHEUS=true; see Step 4c)
```

Headlamp is deployed asynchronously and is **not** part of the `deploy-infra` wait list — a
broken upstream chart release must never block E2E runs. Step 4 below waits for it explicitly
at the point you actually need the UI.

The OpenBao UI is served by the same `openbao` Service that backs the bootstrap scripts; the
kind overlay flips `ui = true` in the standalone Raft config, while the production
flux-system overlay keeps it disabled.

The `openstack` namespace also holds the synced secrets:

```
openstack   keystone-admin        (admin password from OpenBao)
openstack   keystone-db           (database credentials from OpenBao)
```
:::

::: tip Enabling Chaos Mesh
Chaos Mesh is **not installed by default** in the kind Quick Start. The default
`make deploy-infra` flow leaves the `chaos-mesh` namespace absent so first-run
deployments are faster and do not require the `chaos-daemon` privileged
DaemonSet on hosts that don't need it. Production overlays (`deploy/flux-system/`)
also explicitly omit Chaos Mesh.

Opt in by setting `WITH_CHAOS_MESH=true` before `make deploy-infra`:

```bash
WITH_CHAOS_MESH=true make deploy-infra
```

This applies the kind-only overlay at `deploy/kind/chaos-mesh/`, loads the
chaos-daemon kernel modules on the kind node, and waits for the Chaos Mesh
HelmRelease to become Ready alongside the other operators. It is required
before running the chaos E2E suites — see
[Chaos E2E Tests](./reference/testing/chaos-e2e-tests.md) for the full prerequisite
list and `make e2e-chaos` workflow.
:::

::: tip Enabling Prometheus & Grafana
The kube-prometheus-stack is **not installed by default** in the kind Quick Start. The default
`make deploy-infra` flow leaves the `monitoring` namespace absent so first-run
deployments stay lean and do not pin extra CPU/memory on a developer laptop.
Production overlays (`deploy/flux-system/`) also omit the stack — production
clusters wire their own Prometheus.

Opt in by setting `WITH_PROMETHEUS=true` before `make deploy-infra`:

```bash
WITH_PROMETHEUS=true make deploy-infra
```

This applies the kind-only overlay at `deploy/kind/prometheus/`, waits for the
`kube-prometheus-stack` HelmRelease to become Ready, and patches the
keystone-operator HelmRelease to enable its `ServiceMonitor`. Use it when you
want to visualise the keystone-operator metrics live (reconcile p95,
error rate) — see [Step 4c — Open the Grafana UI](#step-4c-grafana-ui) for the
port-forward and the bundled `Keystone Operator` dashboard.
:::

---

## Step 4 — Open the Headlamp UI

The kind overlay ships [Headlamp](https://headlamp.dev/) with the Flux plugin preloaded, so
you can watch Steps 5–9 reconcile live (and, if you enabled `WITH_PROMETHEUS=true`, jump to
[Step 4c](#step-4c-grafana-ui) for the Grafana UI). Wait for the HelmRelease to become Ready, then
get a token, port-forward, and open the UI:

```bash
kubectl wait helmrelease/headlamp \
  -n headlamp-system \
  --for=condition=Ready \
  --timeout=300s

kubectl create token headlamp -n headlamp-system --duration=8h
kubectl port-forward svc/headlamp -n headlamp-system 8080:80
```

Open `http://localhost:8080`, paste the token. The sidebar shows **Flux → Helm Releases /
Kustomizations / Sources** alongside the standard resources. The `headlamp` ServiceAccount is
bound to a read-only ClusterRole covering Flux toolkit API groups and forge-stack CRDs.

### Accessing the Flux UI

The Headlamp Flux plugin is the primary Flux UI used by this project. The `flux-operator`
also ships an embedded Flux Web UI ([fluxoperator.dev/web-ui](https://fluxoperator.dev/web-ui/))
that the kind overlay turns on as a demo addon — see
[Step 4a — Open the Flux Web UI](#step-4a-flux-web-ui) for how to reach it.
Once Headlamp is open and you are authenticated, click **Flux** in the left sidebar
to switch into the Flux views:

| Pane | Shows |
|------|-------|
| **Helm Releases** | Every `HelmRelease` with reconciliation status, last applied revision, and the values that were rendered |
| **Kustomizations** | `Kustomization` objects (empty in the kind Quick Start — the `FluxInstance` here has no `spec.sync` block) |
| **Sources** | `HelmRepository` objects (and `GitRepository`/`OCIRepository` if present) with the last successful fetch and artifact revision |
| **Flux Runtime** | The flux-operator's `FluxInstance/flux` and `FluxReport/flux` — controller versions, reconciliation state, entitlement status |

Use this instead of the legacy `flux get` / `flux logs` CLI — all state the CLI would
print is rendered live here, and every resource row links to the controller logs and
Kubernetes events that produced it.

---

## Step 4a — Open the Flux Web UI {#step-4a-flux-web-ui}

The kind overlay also ships the flux-operator's own
[Flux Web UI](https://fluxoperator.dev/web-ui/) as a demo surface (and, alongside it,
the optional Grafana UI from [Step 4c](#step-4c-grafana-ui) when `WITH_PROMETHEUS=true`).
This is a kind-only convenience — the production `deploy/flux-system/` overlay keeps the
Web UI disabled (no token, no TLS, no Ingress) until the upstream project ships token
auth, TLS termination, and an Ingress story suitable for a shared cluster. Forward the
service port and browse directly:

```bash
kubectl port-forward svc/flux-web -n flux-system 9080:9080
```

Then open <http://localhost:9080> — no login is required.

The Web UI complements Headlamp (Step 4) by rendering the three flux-operator-specific
Custom Resources that the generic Headlamp Flux plugin does not know about: `ResourceSet`
and `ResourceSetInputProvider` (the operator's templating primitives) and `FluxReport`
(the rolled-up installation status of every Flux controller). Standard `HelmRelease`,
`Kustomization`, and `Source` views are still easier to read in Headlamp.

---

## Step 4b — Open the OpenBao UI {#step-4b-openbao-ui}

The kind overlay enables the OpenBao web UI as a demo surface (the optional
Grafana UI is covered separately in [Step 4c](#step-4c-grafana-ui)). This is a
kind-only convenience — the production flux-system overlay keeps `ui = false` in the HA
Raft config. Forward the client port and log in with the root token that
`make deploy-infra` already seeded into the cluster:

```bash
kubectl port-forward svc/openbao -n openbao-system 8200:8200
```

> **Service selection:** `kubectl get svc -n openbao-system` lists two services —
> forward `svc/openbao` (the client `ClusterIP` service that also fronts the UI),
> **not** `svc/openbao-internal` (the headless Service used for Raft peer
> discovery between OpenBao pods).

In a second terminal, extract the root token from the `openbao-init-keys` Secret:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
echo "$BAO_TOKEN"
```

### Present a client certificate (mutual TLS)

The listener enforces **mutual TLS**: every connection must present a client certificate that
chains to the in-cluster CA *before* the application-layer token login runs. A browser without
one never reaches the login screen — the TLS handshake is reset, which the tooling surfaces as
`connection reset by peer` / `lost connection to pod`. The kind overlay matches the production
mTLS posture here.

Build a PKCS#12 bundle from the `openbao-client-tls` Secret. Its keypair is signed by the same
`selfsigned-cluster-issuer` CA as the server cert, and the listener verifies only chain-to-CA
(not SANs) on client auth, so this certificate is accepted:

```bash
kubectl get secret openbao-client-tls -n openbao-system \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > openbao-client.crt
kubectl get secret openbao-client-tls -n openbao-system \
  -o jsonpath='{.data.tls\.key}' | base64 -d > openbao-client.key
kubectl get secret openbao-client-tls -n openbao-system \
  -o jsonpath='{.data.ca\.crt}'  | base64 -d > openbao-ca.crt
# The passphrase only protects this throwaway local .p12 during the browser
# import — it is not an OpenBao credential, so pick any value you like.
openssl pkcs12 -export -inkey openbao-client.key -in openbao-client.crt \
  -certfile openbao-ca.crt -name "OpenBao Client (kind)" \
  -out openbao-client.p12 -passout pass:changeit
```

Import the bundle into Firefox once. This walkthrough is verified with Firefox;
Chrome, Edge, and Safari also accept client certificates but import them through
the OS keychain / system certificate store, so the menu path differs:

1. *Settings → Privacy & Security → Certificates → View Certificates…*
2. Tab **Your Certificates → Import…** → select `openbao-client.p12` and enter the
   passphrase you chose above.
3. *(Optional — removes the trust warning.)* Tab **Authorities → Import…** → select
   `openbao-ca.crt` → tick "Trust this CA to identify websites". The server cert carries
   `IP:127.0.0.1` as a SAN, so `https://localhost:8200` then validates cleanly.

Open `https://localhost:8200/ui/`. Firefox asks which client certificate to send — pick
**"OpenBao Client (kind)"** — then paste the root token to sign in.

For the full token lifecycle, secret engines, auth methods, and the bootstrap sequence that
produced this token, see
[OpenBao Bootstrap Procedure — Running the Full Bootstrap](./reference/infrastructure/openbao-bootstrap.md#running-the-full-bootstrap).

---

## Step 4c — Open the Grafana UI {#step-4c-grafana-ui}

The kind overlay can ship a slimmed-down `kube-prometheus-stack` (Prometheus +
Grafana + the prometheus-operator) under the `monitoring` namespace. This is a
**kind-only opt-in** — production overlays (`deploy/flux-system/`) deliberately
omit the stack so production clusters can wire their own Prometheus. If you
have not already opted in, see the
[Enabling Prometheus & Grafana](#enabling-prometheus--grafana) tip in
Step 3 (`WITH_PROMETHEUS=true make deploy-infra`).

Forward the Grafana service port:

```bash
kubectl port-forward svc/kube-prometheus-stack-grafana -n monitoring 3000:80
```

Open `http://localhost:3000` and sign in with the chart's default credentials:

```
username: admin
password: prom-operator
```

> **Default credentials only:** these are the upstream `kube-prometheus-stack`
> defaults baked into the kind overlay for convenience. Do not reuse them in
> any cluster reachable beyond your laptop.

Once signed in, navigate to **Dashboards → Keystone Operator** to see the
bundled dashboard sourced from `operators/keystone/dashboards/keystone-operator.json`
(reconcile p95, error rate, and the keystone-operator metrics).

### Sanity-check: Prometheus targets

In a second terminal, port-forward Prometheus and confirm the keystone-operator
ServiceMonitor scrape target is `up`. The query path
(`/api/v1/targets?state=active`) is the same one the chainsaw suite
issues against the in-cluster Service — so a green local check matches what
CI exercises:

```bash
kubectl port-forward svc/kube-prometheus-stack-prometheus -n monitoring 9090:9090
curl -fsS 'http://localhost:9090/api/v1/targets?state=active' \
  | jq '.data.activeTargets[] | select(.labels.job=="keystone-operator") | {health, lastScrape: .lastScrape}'
```

A healthy target reports `"health": "up"`; an empty result means the
keystone-operator HelmRelease has not been patched yet — re-run
`WITH_PROMETHEUS=true make deploy-infra` so the deploy script can flip
`monitoring.serviceMonitor.enabled=true`.

For non-kind clusters (production overlays, shared dev clusters, anything that
already runs Prometheus), follow
[Enable Keystone Operator Metrics](./guides/enable-keystone-operator-metrics.md)
instead — that guide covers wiring an externally-managed Prometheus to the
operator's ServiceMonitor and is the canonical non-kind path.

---

## Step 5 — Deploy the Keystone operator

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
  -n keystone-system \
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
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name forge

# 3. Pre-install CRDs so the API server watch cache is ready before the
#    operator starts (avoids missing initial watch events)
kubectl apply -f operators/keystone/helm/keystone-operator/crds/
kubectl wait crd --all --for condition=Established --timeout=60s

# 4. Install the chart
helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  -n keystone-system --create-namespace \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait --timeout 120s
```

### Verify the operator is running

```bash
kubectl get pods -n keystone-system -l app.kubernetes.io/name=keystone-operator
```

Expected output:

```
NAME                                  READY   STATUS    RESTARTS   AGE
keystone-operator-6d7f9f4d5b-abc12   1/1     Running   0          30s
keystone-operator-6d7f9f4d5b-xyz99   1/1     Running   0          30s
```

---

## Step 6 — Build and load the Keystone service image

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
kind load docker-image ghcr.io/c5c3/keystone:"${RELEASE}" --name forge
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
kind load docker-image "ghcr.io/c5c3/keystone:${RELEASE}" --name forge
```

---

## Step 7 — Create a Keystone CR

Apply the following manifest to deploy a Keystone instance in **managed mode**. In this mode the
operator creates and manages the MariaDB database (via `clusterRef`) and configures Memcached
for session caching. The `spec.gateway` block attaches the Keystone API to the
`openstack-gw` Gateway provisioned in Step 2b, so the service is reachable at
`https://keystone.127-0-0-1.nip.io/v3` from your workstation with no port-forward.
Replace `<RELEASE>` with the same value used in Step 6 (e.g. `2025.2`):

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
    tag: "<RELEASE>"   # e.g. 2025.2 — must match the image loaded in Step 6
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
  # Gateway API attachment — routes https://keystone.127-0-0-1.nip.io/v3 to
  # the keystone Service via the shared openstack-gw Gateway.
  gateway:
    parentRef:
      name: openstack-gw
    hostname: keystone.127-0-0-1.nip.io
    path: /
```

::: tip Why `127-0-0-1.nip.io`?
[nip.io](https://nip.io/) is a free wildcard DNS service: any hostname of the form
`anything.<ip-with-dashes>.nip.io` resolves to `<ip>`. `keystone.127-0-0-1.nip.io`
therefore resolves to `127.0.0.1` without touching `/etc/hosts`, which pairs with
the `hostPort: 443 → containerPort: 31443` mapping in `hack/kind-config.yaml`.
:::

```bash
kubectl apply -f keystone.yaml
```

::: details Variant — when `KIND_HOST_PORT=8443` was used in Step 2
If you exported `KIND_HOST_PORT=8443` before `make deploy-infra`, the
endpoint is reachable at `https://keystone.127-0-0-1.nip.io:8443/v3`
instead of the default `:443`. The Gateway API `hostname` field stays
unchanged (Gateway API hostnames carry no port — the HTTPRoute matches
the SNI / Host header), but `spec.bootstrap.publicEndpoint` must be set
explicitly so the issued service catalog points at the same `:8443` URL
external clients reach. Apply this CR instead:

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
    tag: "<RELEASE>"   # e.g. 2025.2 — must match the image loaded in Step 6
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
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
    # Catalog URL must include the host-side port that KIND_HOST_PORT set.
    publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
  gateway:
    parentRef:
      name: openstack-gw
    hostname: keystone.127-0-0-1.nip.io   # no port — Gateway API hostnames are SNI/Host only
    path: /
```

When verifying access (see [Access Keystone from your local machine](#access-keystone-from-your-local-machine)),
substitute `:8443` everywhere the default guide writes `https://keystone.127-0-0-1.nip.io/v3`,
e.g. `export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3`.

`kubectl get keystone` will report the same `:8443` URL in the `ENDPOINT`
column once the CR reconciles: the operator mirrors
`spec.bootstrap.publicEndpoint` into `status.endpoint` whenever it is set,
so the externally reachable URL, the issued service catalog, and
`status.endpoint` stay aligned. The webhook rejects a
`publicEndpoint` whose host differs from `spec.gateway.hostname`, so
drift between the catalog URL and the HTTPRoute hostname is caught at
admission time.
:::

---

## Step 8 — Wait for Keystone to become Ready

The operator reconciles the CR through fourteen sub-conditions before the
aggregate `Ready` condition is set — all are always reported; conditions tied to
an optional spec field carry a "not required" / "disabled" reason when that
field is unset:

| Condition | What it waits for |
|-----------|-------------------|
| `SecretsReady` | `keystone-db` and `keystone-admin` Secrets are available |
| `FernetKeysReady` | Fernet key Secret and CronJob created |
| `CredentialKeysReady` | Credential key Secret and rotation CronJob exist (`spec.credentialKeys` tunes schedule and max active keys) |
| `DatabaseReady` | `db_sync` Job completed successfully |
| `DatabaseTLSReady` | Database TLS client certificate issued, or `NotRequired` when `spec.database.tls` is unset |
| `PolicyValidReady` | `spec.policyOverrides` validated against `oslo.policy` |
| `DeploymentReady` | Keystone API Deployment has available replicas |
| `KeystoneAPIReady` | Keystone API is responding to `/v3` health probes |
| `HPAReady` | HorizontalPodAutoscaler created (if `spec.autoscaling` is set) |
| `NetworkPolicyReady` | NetworkPolicy created (if `spec.networkPolicy` is set) |
| `HTTPRouteReady` | Gateway API HTTPRoute reconciled, or not required when `spec.gateway` is unset |
| `BootstrapReady` | Bootstrap Job completed (admin user, region, endpoints) |
| `TrustFlushReady` | Trust-flush CronJob created (defaults to hourly) |
| `PasswordRotationReady` | Scheduled admin-password rotation reconciled, or `RotationDisabled` when `spec.passwordRotation` is unset |

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

## Step 9 — Verify the deployment

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
  "import urllib.request; print(urllib.request.urlopen('http://keystone.openstack.svc:5000/v3').read().decode())"
```

A successful response returns a JSON document beginning with `{"version": {"id": "v3", ...}}`.

### Inspect conditions in detail

```bash
kubectl get keystone keystone -n openstack -o jsonpath='{.status.conditions}' | jq .
```

---

## Access Keystone from your local machine

The Keystone CR from Step 7 attaches to the `openstack-gw` Gateway and is exposed at
`https://keystone.127-0-0-1.nip.io/v3`. The kind cluster's `extraPortMappings` bridges the
host's TCP :443 to the Envoy proxy's NodePort `31443`, so the endpoint resolves directly
to `127.0.0.1` via the [nip.io](https://nip.io/) wildcard DNS service — no `/etc/hosts`
edit and no `kubectl port-forward` required.

::: warning Did you set `KIND_HOST_PORT=8443` in Step 2?
Then the endpoint is `https://keystone.127-0-0-1.nip.io:8443/v3` instead of the
default `:443`. Substitute `:8443` everywhere this section writes
`https://keystone.127-0-0-1.nip.io/...`, including `OS_AUTH_URL`, the verification
`curl`, and any URL printed by `openstack catalog`. The Gateway hostname and the
extracted CA from Option B are unchanged — only the port differs. The matching
`spec.bootstrap.publicEndpoint` is shown in the `KIND_HOST_PORT=8443` variant
block at the end of Step 7.
:::

### Step 1 — Export OpenStack credentials

Read the admin password directly from the cluster and set the standard OpenStack
environment variables against the nip.io endpoint:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
```

### Step 2 — Verify access

Check that the API responds and that authentication works:

```bash
# Unauthenticated version endpoint (-k skips verification; see below)
curl -k https://keystone.127-0-0-1.nip.io/v3

# Authenticated token request (requires python-openstackclient)
openstack --insecure token issue
```

A successful `curl` response begins with `{"version": {"id": "v3", ...}}`. A successful
`openstack token issue` prints a table with the issued token and its expiry.

> **Note:** The service catalog returned by Keystone contains cluster-internal endpoint URLs
> (e.g. `http://keystone.openstack.svc.cluster.local:5000/v3`). Only `identity` commands
> that authenticate directly against `OS_AUTH_URL` work through the Gateway. Commands that
> resolve other service endpoints from the catalog (Nova, Neutron, …) require additional
> Gateway routes or port-forwards for each service.

### Accept the self-signed certificate

The Gateway terminates TLS with a certificate issued by the in-cluster
`selfsigned-cluster-issuer`, which is not in any OS trust store. Pick one of:

**Option A — quick, one-off runs:** pass `-k` / `--insecure` on the CLI.

```bash
curl -k https://keystone.127-0-0-1.nip.io/v3
openstack --insecure token issue
# Or set the environment variable once per shell:
export OS_INSECURE=true
```

**Option B — trust the issuer (recommended for repeated use):** extract the
self-signed CA from the cluster and point OpenStack tooling at it.

```bash
# Extract the issuer's CA bundle to a local file
kubectl get secret keystone-nip-io-tls -n openstack \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > keystone-ca.crt
export OS_CACERT="$(pwd)/keystone-ca.crt"

# Now curl and the OpenStack CLI accept the cert
curl --cacert "$OS_CACERT" https://keystone.127-0-0-1.nip.io/v3
openstack token issue
```

### Fallback — `kubectl port-forward` {#fallback-kubectl-port-forward}

If the Gateway is unavailable (Envoy still reconciling, nip.io blocked by a corporate
resolver, or you are debugging the in-cluster Service directly), fall back to
port-forwarding the `keystone` Service and pointing `OS_AUTH_URL` at `localhost`:

```bash
# In a separate terminal:
kubectl port-forward svc/keystone -n openstack 5000:5000

# Then, in your shell:
export OS_AUTH_URL=http://localhost:5000/v3
curl http://localhost:5000/v3
openstack token issue
```

This reaches the ClusterIP Service directly and bypasses the Gateway data plane entirely.

---

## Next steps

Keystone is running, you can reach the API, and you have admin credentials. The three
follow-up guides below cover everything you will actually do with the CR:

| Guide | When to read it |
|-------|-----------------|
| [Observability & Diagnostics](./guides/observability.md) | First stop when something is not `Ready` — how to read conditions, events, and status fields |
| [Day 2 Operations](./guides/day-2-operations.md) | Scale, upgrade the OpenStack release, rotate Fernet keys manually |
| [Advanced Configuration](./guides/advanced-configuration.md) | Brownfield database, autoscaling, network policy, free-form INI, and pointers to every other `spec.*` option |

For the full field reference of the Keystone CR, see
[Keystone CRD API Reference](./reference/keystone/keystone-crd.md).

---

## Running the E2E test suite

The project ships a full [Chainsaw](https://kyverno.github.io/chainsaw/) test suite that validates
all operator behaviour. With the cluster running from the steps above, execute:

```bash
# The entire tests/e2e/ tree — 45 suites across keystone, keystone-operator,
# c5c3 (ControlPlane), and infrastructure — up to 4 in parallel
make e2e

# Or target a specific suite
chainsaw test \
  --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/keystone/basic-deployment/
```

A representative selection of the suites under `tests/e2e/keystone/` (the
directory itself is the canonical inventory; see the
[E2E test reference](./reference/testing/keystone-e2e-tests.md) for the full
catalogue):

| Suite | What it validates |
|-------|-------------------|
| `basic-deployment` | Full happy-path reconciliation, API accessibility |
| `missing-secret` | Recovery when a referenced Secret is absent |
| `fernet-rotation` | Manual key rotation with in-place delivery (no rollout) |
| `scale` | Replicas scale from 3 → 5 → 2 correctly |
| `deletion-cleanup` | All owned resources are garbage-collected on CR deletion |
| `policy-overrides` | `oslo.policy` ConfigMap integration |
| `middleware-config` | WSGI `api-paste.ini` middleware pipeline customization |
| `brownfield-database` | Explicit database host (no MariaDB CR created) |
| `image-upgrade` | Rolling image update with zero downtime |
| `invalid-cr` | Webhook rejects invalid cron expressions and duplicate plugins |

JUnit XML reports are written to `_output/reports/` after each run.

---

## Running Tempest API tests

With the Keystone CR Ready from Step 8, validate the identity API with the OpenStack Tempest
test suite:

```bash
SERVICE=keystone hack/run-tempest.sh
```

The script handles everything automatically:

1. Builds the Tempest container image from the pinned versions in `releases/2025.2/test-refs.yaml`
2. Establishes a port-forward to the Keystone API (skipped if one is already running)
3. Runs the identity tests defined in `tests/tempest/keystone-2025-2/`

Results land in `_output/tempest/`:

| File | Content |
|------|---------|
| `tempest-results.xml` | JUnit XML — open in your IDE or import into a CI viewer |
| `tempest.subunit` | Raw subunit stream for further processing |

::: tip Faster re-runs
The image build is skipped when `BUILD_IMAGE=false`, which saves ~30 s on repeated runs:

```bash
BUILD_IMAGE=false SERVICE=keystone hack/run-tempest.sh
```
:::

---

## Teardown

Delete the kind cluster and all its resources:

```bash
make teardown-infra
```

This runs `kind delete cluster --name forge` and removes all local state.
