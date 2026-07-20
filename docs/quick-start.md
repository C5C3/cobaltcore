---
title: Quick Start
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start

Deploys Keystone, OpenStack's identity service, to a local kind cluster on
**macOS** on host port **`8443`**, which avoids the `vmnetd` helper and
privileged-port binding that kind's default port `443` needs (see
[Quick Start (Extended) — Step 2](./quick-start-extended.md#step-2-create-the-kind-cluster)
for why). For UI tours, fallbacks, the local-build path, the production
HelmRelease, E2E and Tempest, see
[Quick Start (Extended)](./quick-start-extended.md).

## Prerequisites

- A running Docker daemon (Docker Desktop, or a Docker-API-compatible
  alternative such as Colima, OrbStack, or Rancher Desktop; see
  [Quick Start (Extended) — Step 2](./quick-start-extended.md#step-2-create-the-kind-cluster)
  for the privileged-port caveats specific to each)
- [Helm](https://helm.sh/docs/intro/install/) and
  [jq](https://jqlang.org/download/), each via its platform installer:
  neither is installed by the script below
- [openstack CLI](https://docs.openstack.org/python-openstackclient/latest/installation.html)
  (`python-openstackclient`), needed only for the token-issue check in
  Step 6
- Pinned `kind` and `kubectl` on `PATH`. `make deploy-infra` calls both by
  their bare command name, so they must resolve on `PATH`, not just exist
  in `~/.local/bin`:

  ```bash
  make install-test-deps
  export PATH="${HOME}/.local/bin:${PATH}"
  ```

- `yq` on `PATH` (only required because `KIND_HOST_PORT` is overridden)
- A stable internet connection: `make deploy-infra` pulls container images
  and Helm chart sources over the network. If a step times out partway
  through (for example while waiting on the MariaDB CR), run
  `make teardown-infra` and retry from Step 2 rather than resuming a
  half-provisioned cluster.

## Step 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## Step 2 — Cluster + infrastructure stack

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Creates the `forge` kind cluster with `host:8443 → nodePort 31443`,
then installs Flux, cert-manager, the Gateway API CRDs,
prometheus-operator-crds, OpenBao (initialised, unsealed and bootstrapped),
MariaDB operator + `openstack-db`, External Secrets, Memcached operator +
`openstack-memcached`, Envoy Gateway and the shared `openstack-gw`. Expect
**5–10 minutes** on first run: each component above pulls its own image
from its upstream registry on a cold Docker cache.

## Step 3 — Keystone operator

Registers the Flux `HelmRepository` for the c5c3 charts, then applies a
`HelmRelease` that installs the keystone-operator chart and waits for Flux
to report it `Ready`. This is the controller that reconciles the `Keystone`
CR you apply in Step 5.

```bash
kubectl apply -f deploy/flux-system/sources/c5c3-charts.yaml
kubectl apply -f deploy/flux-system/releases/keystone-operator.yaml
kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=120s
```

## Step 4 — Keystone service image

Pulls the pre-built Keystone service image (the actual OpenStack Keystone
API server, distinct from the operator installed in Step 3) and loads it
into the kind cluster's local image store so the Deployment created in
Step 5 starts without a registry pull.

> Note: the keystone-operator controller runs in `keystone-system`; the Keystone workload it manages runs in `openstack` (controller-vs-workload split).

```bash
RELEASE=2025.2  # the release this Quick Start's bundled CR is pinned to,
                # not necessarily the newest one the repo supports — see
                # releases/ for every release the operator builds today
docker pull ghcr.io/c5c3/keystone:${RELEASE}
kind load docker-image ghcr.io/c5c3/keystone:${RELEASE} --name forge
```

## Step 5 — Keystone CR

Applies a `Keystone` custom resource. The operator reconciles it into a
Deployment, Service, Fernet key rotation, and the bootstrap Job that
creates the `admin` user, the `RegionOne` region, and the service catalog
entries below.

```yaml
# keystone.yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/keystone
    tag: "2025.2"
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
    publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
  gateway:
    parentRef:
      name: openstack-gw
    hostname: keystone.127-0-0-1.nip.io
    path: /
```

```bash
kubectl apply -f keystone.yaml
kubectl wait keystone/keystone -n openstack \
  --for=condition=Ready --timeout=5m
```

## Step 6 — Verify

Confirms the API is reachable and issues an authenticated token, proving
the bootstrap admin credentials from Step 5 work end to end.

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Expected output:

```json
{"version": {"id": "v3.14", "status": "stable", "updated": "2020-04-07T00:00:00Z", "links": [{"rel": "self", "href": "https://keystone.127-0-0-1.nip.io:8443/v3/"}], "media-types": [{"base": "application/json", "type": "application/vnd.openstack.identity-v3+json"}]}}
```

Authenticated token request:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

Expected output:

```text
+------------+-------------------------------------------------------------------------------------------------------+
| Field      | Value                                                                                                 |
+------------+-------------------------------------------------------------------------------------------------------+
| expires    | 2026-04-27T10:47:07+0000                                                                              |
| id         | gAAAAABp7zCb9zhkS7ULijkujyqTFwXQshf_SXm6TMe0APpwHCpTV10gGrEakgWX-                                     |
|            | OKcFgwDocxHvluFfr9MN2ByqSmuMEJT2vuXfTbOX7mn1zMIecvUTwLFQKgWsKpfQyRFNW71s4S4MVpd93o_EPLleg7aAZPT-      |
|            | fLjitIFzU7b6sCSUG-CEdg                                                                                |
| project_id | aed71e82de764a00aaab396e472e7929                                                                      |
| user_id    | 8ac0e4e97079469dacfd1c5732c6e06b                                                                      |
+------------+-------------------------------------------------------------------------------------------------------+
```

## Optional — UIs

**Headlamp** (Kubernetes + Flux dashboard):

```bash
kubectl wait helmrelease/headlamp -n headlamp-system \
  --for=condition=Ready --timeout=300s
kubectl create token headlamp -n headlamp-system --duration=8h
kubectl port-forward svc/headlamp -n headlamp-system 8080:80
```

Open <http://localhost:8080> and paste the token.

**OpenBao** (root token from `openbao-init-keys`):

```bash
kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token'
kubectl port-forward svc/openbao -n openbao-system 8200:8200
```

The listener enforces mutual TLS, so the browser must present a client
certificate before it reaches the UI. Build a PKCS#12 bundle from the
`openbao-client-tls` Secret, import it into Firefox, and sign in with the
token; see [Extended Quick Start — Step 4b](./quick-start-extended.md#step-4b-openbao-ui)
for the exact commands, CA trust, and browser notes.

> **Grafana (kind-only, opt-in):** for the keystone-operator metrics dashboard, run `WITH_PROMETHEUS=true make deploy-infra` and follow [Extended Quick Start — Step 4c](./quick-start-extended.md#step-4c-grafana-ui). The compact path stays Grafana-free by default.

## Teardown

```bash
make teardown-infra
```
