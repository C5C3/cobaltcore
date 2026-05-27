<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start

The shortest path from `git clone` to an authenticated Keystone API call on
**macOS** with kind on host port **`8443`** — no `vmnetd` helper, no
privileged port binding. For UI tours, fallbacks, the local-build path,
the production HelmRelease, E2E and Tempest, see
[Quick Start (Extended)](./quick-start-extended.md).

## Prerequisites

- Docker Desktop running
- Pinned `kind`, `kubectl`, `Helm`, `jq` on `PATH`:

  ```bash
  make install-test-deps
  export PATH="${HOME}/.local/bin:${PATH}"
  ```

- `yq` on `PATH` (only required because `KIND_HOST_PORT` is overridden)

## Step 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## Step 2 — Cluster + infrastructure stack

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Creates the `forge-e2e` kind cluster with `host:8443 → nodePort 31443`,
then installs Flux, cert-manager, OpenBao (initialised, unsealed and
bootstrapped), MariaDB operator + `openstack-db`, Memcached operator +
`openstack-memcached`, External Secrets, Envoy Gateway and the shared
`openstack-gw`. Expect **5–10 minutes** on first run (image pulls dominate).

## Step 3 — Keystone operator

```bash
kubectl apply -f deploy/flux-system/sources/c5c3-charts.yaml
kubectl apply -f deploy/flux-system/releases/keystone-operator.yaml
kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=120s
```

## Step 4 — Keystone service image

> Note: the keystone-operator controller runs in `keystone-system`; the Keystone workload it manages runs in `openstack` (controller-vs-workload split, CC-0105).

```bash
RELEASE=2025.2
docker pull ghcr.io/c5c3/keystone:${RELEASE}
kind load docker-image ghcr.io/c5c3/keystone:${RELEASE} --name forge-e2e
```

## Step 5 — Keystone CR

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

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Erwartete Ausgabe:

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

Erwartete Ausgabe:

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

Open <https://localhost:8200/ui/>, accept the self-signed cert warning,
paste the token.

> **Grafana (kind-only, opt-in):** for the keystone-operator metrics dashboard, run `WITH_PROMETHEUS=true make deploy-infra` and follow [Extended Quick Start — Step 4c](./quick-start-extended.md#step-4c-grafana-ui). The compact path stays Grafana-free by default.

## Teardown

```bash
make teardown-infra
```
