<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start (ControlPlane): C5C3 + K-ORC on Kind

This guide brings up the **c5c3 ControlPlane** — the top-level aggregate that
projects a full Keystone control plane from a single CR — together with
[K-ORC](https://github.com/k-orc/openstack-resource-controller) (the OpenStack
Resource Controller), which the c5c3-operator drives to mint and rotate the
admin application credential and to register the identity service catalog.

Where the [Quick Start](./quick-start.md) and
[Quick Start (Extended)](./quick-start-extended.md) stop at a hand-applied
`Keystone` CR (the per-service operator), this page goes one level up: a single
`ControlPlane` CR makes the c5c3-operator provision the `MariaDB`, `Memcached`,
and `Keystone` children, mint the K-ORC admin application credential, mirror it to
OpenBao, and register the K-ORC `Service` / `Endpoint` catalog — all reconciled
link-by-link to an aggregate `Ready`.

::: tip One opt-in flag brings up the whole stack
`WITH_CONTROLPLANE=true make deploy-infra` deploys the full ControlPlane stack
(keystone-operator, K-ORC, c5c3-operator) from the **published** charts plus the
pinned K-ORC installer, and applies a `ControlPlane` CR for you. In this mode
deploy-infra does **not** create the shared MariaDB/Memcached — the ControlPlane
provisions them itself (managed mode), as designed. The plain `make deploy-infra`
(without the flag) leaves the ControlPlane stack suspended so the per-service
Keystone Quick Start path stays light.
:::

## Prerequisites

Same toolchain as the [Quick Start](./quick-start.md), and an internet connection
(the K-ORC source is cloned from GitHub at a pinned tag):

- Docker Desktop running, with enough resources for the full stack — the
  ControlPlane provisions its own MariaDB with the operator's default
  (production-shaped, Galera) topology, which is heavier than the single-replica
  database the per-service Quick Start patches in. Give Docker ample CPU/memory.
- Pinned `kind`, `kubectl`, `Helm`, `jq`, `yq` on `PATH`:

  ```bash
  make install-test-deps
  export PATH="${HOME}/.local/bin:${PATH}"
  ```

## Step 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## Step 2 — Cluster + ControlPlane stack

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

`KIND_HOST_PORT=8443` maps the Gateway to a non-privileged host port so the
Keystone API is reachable at `https://keystone.127-0-0-1.nip.io:8443/v3` on
macOS — the same convention as the [Quick Start](./quick-start.md). On Linux
with rootful Docker you can drop the override (the default port `443` works) and
use `https://keystone.127-0-0-1.nip.io/v3` everywhere below. deploy-infra sets
the ControlPlane's `publicEndpoint` to match whichever port it brought the
cluster up with.

This creates the `forge-e2e` kind cluster, the shared infrastructure
(cert-manager, OpenBao initialised/unsealed/bootstrapped, the MariaDB and
Memcached operators, External Secrets, Envoy Gateway and the shared
`openstack-gw`), and then — because `WITH_CONTROLPLANE=true` — the ControlPlane
stack on top:

- deploys **keystone-operator**, **K-ORC** (a Flux `GitRepository` +
  `Kustomization` over the pinned `v2.5.0` installer), and **c5c3-operator** from
  the published charts — nothing is built locally;
- does **not** create the `openstack-db` MariaDB / `openstack-memcached` Memcached
  CRs — the ControlPlane provisions them;
- seeds a password-based admin `clouds.yaml` in OpenBao and materialises
  `k-orc-clouds-yaml` via ESO into `openstack` (where the projected K-ORC CRs
  resolve it) and `orc-system`;
- applies a `ControlPlane` CR (`deploy/kind/controlplane`) that drives the chain.

Expect **5–10 minutes** for the infrastructure; the ControlPlane chain then
reconciles in the background (Step 3).

::: tip The OpenBao seed avoids a chicken-and-egg deadlock
K-ORC needs a `clouds.yaml` to authenticate, but the application credential it
should use does not exist until the c5c3-operator mints it through K-ORC. The
bootstrap seeds a *password-based* `clouds.yaml` so the first reconcile can
authenticate; once the operator mints the application credential, its PushSecret
overwrites the OpenBao path with the credential-based `clouds.yaml` and ESO
re-materialises it.
:::

## Step 3 — Watch the chain reconcile

The aggregate `Ready` becomes `True` only after all five sub-conditions are
`True`, gated in dependency order:

```
InfrastructureReady → KeystoneReady → KORCReady → AdminCredentialReady → CatalogReady
```

Watch the ControlPlane and its sub-conditions:

```bash
kubectl get controlplane controlplane -n openstack -w
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

Inspect the projected children:

```bash
# Infrastructure + Keystone provisioned by the ControlPlane
kubectl get mariadb,memcached,keystone -n openstack

# K-ORC resources the operator drives (group openstack.k-orc.cloud)
kubectl get applicationcredentials,services,endpoints.openstack.k-orc.cloud -n openstack
```

If `AdminCredentialReady` lingers on `WaitingForCloudsYaml`, force the
`k-orc-clouds-yaml` ExternalSecret to sync (it is not in the deploy-infra wait
list, so it may otherwise refresh only on its hourly interval):

```bash
kubectl annotate externalsecret/k-orc-clouds-yaml -n openstack \
  force-sync="$(date +%s)" --overwrite
```

Then wait for the aggregate condition:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=15m
```

## Step 4 — Verify

**The minted application credential** — the operator mints one restricted admin
application credential through K-ORC and records its ID on the ControlPlane
status:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.adminApplicationCredential}' | jq .
kubectl get applicationcredentials.openstack.k-orc.cloud -n openstack
```

**The OpenBao round-trip** — the minted credential is pushed to OpenBao and
re-materialised; a `SecretSynced` ExternalSecret confirms the loop closed:

```bash
kubectl get externalsecret k-orc-clouds-yaml -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
```

**The identity catalog** — the catalog sub-reconciler registers the Keystone
identity `Service` and public `Endpoint` as K-ORC CRs:

```bash
kubectl get services.openstack.k-orc.cloud,endpoints.openstack.k-orc.cloud -n openstack
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.catalogReady}{"\n"}'
```

**An authenticated token** — the ControlPlane exposes the projected Keystone
externally through the shared Envoy Gateway (`services.keystone.gateway` points
at `openstack-gw` with hostname `keystone.127-0-0-1.nip.io`), so it is reachable
from the host at `https://keystone.127-0-0-1.nip.io:8443/v3` — the same path as
the per-service [Quick Start](./quick-start.md), no port-forward. Check the
version document first:

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Then issue a token with the admin password:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

> The `:8443` host port matches `KIND_HOST_PORT` from Step 2. With the default
> `KIND_HOST_PORT=443` use `https://keystone.127-0-0-1.nip.io/v3` instead.

> **In-cluster fallback (no Gateway):** if you drive your own `ControlPlane`
> that omits `services.keystone.gateway`, the projected Keystone is reachable
> only via its ClusterIP Service. Port-forward it and use the in-cluster URL:
> ```bash
> kubectl port-forward svc/controlplane-keystone -n openstack 5000:5000
> export OS_AUTH_URL=http://localhost:5000/v3
> ```
> The Service is named `{controlplane-name}-keystone` (`controlplane-keystone`
> for the deploy-infra CR); confirm with `kubectl get svc -n openstack`.

## Customising the ControlPlane

`WITH_CONTROLPLANE=true` applies the CR at `deploy/kind/controlplane/controlplane.yaml`.
To drive your own, edit that manifest (or apply a different `ControlPlane` CR after
the bring-up) — see the [ControlPlane CRD API Reference](./reference/c5c3/controlplane-crd.md)
for every `spec.*` field.

## Teardown

```bash
make teardown-infra
```

## Related references

- [ControlPlane CRD API Reference](./reference/c5c3/controlplane-crd.md) — every
  `spec.*` field, the webhooks, and the status conditions.
- [ControlPlane Reconciler](./reference/c5c3/controlplane-reconciler.md) — the
  sub-reconciler ordering and gating semantics.
- [Quick Start](./quick-start.md) — the compact per-service Keystone path.
