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

## 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## 2 — Cluster + infrastructure stack

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Creates the `forge-e2e` kind cluster with `host:8443 → nodePort 31443`,
then installs Flux, cert-manager, OpenBao (initialised, unsealed and
bootstrapped), MariaDB operator + `openstack-db`, Memcached operator +
`openstack-memcached`, External Secrets, Envoy Gateway and the shared
`openstack-gw`. Expect **5–10 minutes** on first run (image pulls dominate).

## 3 — Keystone operator

```bash
kubectl apply -f deploy/flux-system/sources/c5c3-charts.yaml
kubectl apply -f deploy/flux-system/releases/keystone-operator.yaml
kubectl wait helmrelease/keystone-operator -n openstack \
  --for=condition=Ready --timeout=120s
```

## 4 — Keystone service image

```bash
RELEASE=2025.2
docker pull ghcr.io/c5c3/keystone:${RELEASE}
kind load docker-image ghcr.io/c5c3/keystone:${RELEASE} --name forge-e2e
```

## 5 — Keystone CR

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

## 6 — Verify

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Returns `{"version": {"id": "v3", ...}}`.

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
kubectl port-forward svc/openbao -n openbao-system 8200:8200
# in a second terminal:
kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token'
```

Open <https://localhost:8200/ui/>, accept the self-signed cert warning,
paste the token.

## Teardown

```bash
make teardown-infra
```
