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
`Keystone` CR (the per-service operator), this page goes one level up: you apply
a single `ControlPlane` CR and the c5c3-operator projects the `MariaDB`,
`Memcached`, and `Keystone` children, mints the K-ORC admin application
credential, mirrors it to OpenBao, and registers the K-ORC `Service` /
`Endpoint` catalog — all reconciled link-by-link to an aggregate `Ready`.

::: warning Local-build path — published artifacts are a pending fast-follow
The published c5c3-operator Helm chart (GHCR OCI) is **not yet reachable**, so
`make deploy-infra` deliberately **suspends** the `c5c3-operator` and `k-orc`
Flux objects in kind (the K-ORC source is now a `GitRepository` + `Kustomization`
over the upstream installer, but it stays suspended until the full chain is wired
to run on a cluster). This guide therefore uses the **local-build** path: it
builds and installs the c5c3-operator from the in-repo Helm chart and installs
K-ORC from its upstream release manifest. Once the chart publishes to GHCR and
the suspend is lifted from `hack/deploy-infra.sh`, the `ControlPlane` becomes a
Flux happy-path (`kubectl apply -f` the manifests, exactly like the
keystone-operator in the [Quick Start](./quick-start.md)) and this manual
assembly is no longer needed.

The end-to-end ControlPlane chain on kind is wired here by hand; the in-repo
`tests/e2e/c5c3/full-controlplane-keystone` Chainsaw suite still **skips** when
this stack is absent from the default kind bring-up. Treat this walkthrough as
the manual recipe for that deferred wiring, not a CI-gated happy-path.
:::

## Prerequisites

Same toolchain as the [Quick Start](./quick-start.md), plus a working internet
connection for the upstream K-ORC release manifest:

- Docker Desktop running
- Pinned `kind`, `kubectl`, `Helm`, `jq`, `yq` on `PATH`:

  ```bash
  make install-test-deps
  export PATH="${HOME}/.local/bin:${PATH}"
  ```

`yq` is required because this guide overrides `KIND_HOST_PORT`.

## Step 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## Step 2 — Cluster + infrastructure stack

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

This creates the `forge-e2e` kind cluster and the full shared stack the
ControlPlane depends on: cert-manager, OpenBao (initialised, unsealed, and
bootstrapped), the MariaDB and Memcached operators, External Secrets, and Envoy
Gateway. Expect **5–10 minutes** on first run.

Beyond the stack the compact Quick Start uses, this step also prepares the
ControlPlane-specific pieces:

- the `c5c3-system` and `orc-system` namespaces are created;
- the OpenBao bootstrap **seeds a password-based `clouds.yaml`** at the path
  `openstack/keystone/admin/app-credential` so the admin credential exists
  before the operator mints the application credential;
- the `k-orc-clouds-yaml` ExternalSecret is applied into **both** `openstack`
  (where the projected K-ORC CRs resolve it) and `orc-system` (K-ORC's global
  default mount);
- the `c5c3-operator` and `k-orc` Flux releases are left **suspended** — you
  install both yourself in Steps 4 and 5.

::: tip The seed avoids a chicken-and-egg deadlock
K-ORC needs a `clouds.yaml` to authenticate, but the application credential it
should use does not exist until the c5c3-operator mints it through K-ORC. The
bootstrap seeds a *password-based* `clouds.yaml` so the first reconcile can
authenticate; once the operator mints the application credential, its PushSecret
overwrites the OpenBao path with the credential-based `clouds.yaml`, and ESO
re-materialises it.
:::

## Step 3 — Keystone operator + service image

The ControlPlane projects a `Keystone` CR, so the keystone-operator must be
running and the Keystone service image must be loadable in the cluster.

Re-apply the keystone-operator release file directly (this clears the kind
`suspend` patch, so Flux installs the published chart and reconciles it):

```bash
kubectl apply -f deploy/flux-system/sources/c5c3-charts.yaml
kubectl apply -f deploy/flux-system/releases/keystone-operator.yaml
kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=120s
```

Load the Keystone service image the projected CR references
(`ghcr.io/c5c3/keystone:<release>`):

```bash
RELEASE=2025.2
docker pull ghcr.io/c5c3/keystone:${RELEASE}
kind load docker-image ghcr.io/c5c3/keystone:${RELEASE} --name forge-e2e
```

## Step 4 — Install K-ORC

K-ORC is installed from its upstream release manifest into the `orc-system`
namespace (its default). The manifest ships the K-ORC CRDs
(`applicationcredentials`, `services`, `endpoints`, … in the
`openstack.k-orc.cloud` group) and the controller-manager:

```bash
# Latest release; pin a tag (…/download/v2.5.0/install.yaml) for reproducibility.
kubectl apply --server-side \
  -f https://github.com/k-orc/openstack-resource-controller/releases/latest/download/install.yaml

kubectl wait --for=condition=Available deployment --all \
  -n orc-system --timeout=120s
```

Confirm the CRDs the c5c3-operator drives are registered:

```bash
kubectl get crd applicationcredentials.openstack.k-orc.cloud \
  services.openstack.k-orc.cloud endpoints.openstack.k-orc.cloud
```

::: tip K-ORC credential resolution
Each K-ORC CR the c5c3-operator projects carries its own
`cloudCredentialsRef`, which K-ORC resolves in the **CR's own namespace**
(`openstack`) — that is the `k-orc-clouds-yaml` Secret materialised in Step 2.
The copy in `orc-system` only backs K-ORC's global default mount and is not on
the ControlPlane's critical path.
:::

## Step 5 — Build and install the c5c3-operator

The published chart is not on GHCR yet, so build the operator image, load it
into kind, and install the in-repo chart. The chart's validating/defaulting
webhook is issued a certificate by cert-manager (installed in Step 2):

```bash
# 1. Build the operator image
make docker-build OPERATOR=c5c3 IMG=ghcr.io/c5c3/c5c3-operator:dev

# 2. Load it into the kind cluster (no registry needed)
kind load docker-image ghcr.io/c5c3/c5c3-operator:dev --name forge-e2e

# 3. Pre-install the CRDs so the API server watch cache is ready
kubectl apply -f operators/c5c3/helm/c5c3-operator/crds/
kubectl wait crd controlplanes.c5c3.io \
  --for condition=Established --timeout=60s

# 4. Install the chart into c5c3-system
helm install c5c3-operator \
  operators/c5c3/helm/c5c3-operator/ \
  -n c5c3-system --create-namespace \
  --set image.repository=ghcr.io/c5c3/c5c3-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait --timeout 120s
```

Verify the operator is running:

```bash
kubectl get pods -n c5c3-system -l app.kubernetes.io/name=c5c3-operator
```

## Step 6 — Apply the ControlPlane CR

This single CR drives the whole chain. It runs in **managed mode**: the
`clusterRef` names match the clusters the deploy stack provisions, so the
operator projects an owned `MariaDB` (`openstack-db`) and `Memcached`
(`openstack-memcached`) in the `openstack` namespace, then a `{name}-keystone`
Keystone CR, then the K-ORC admin application credential and the identity
catalog.

```yaml
# controlplane.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  region: RegionOne
  infrastructure:
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
  services:
    keystone:
      replicas: 3
  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
        secretName: k-orc-clouds-yaml
      passwordSecretRef:
        name: keystone-admin
        key: password
      applicationCredential:
        restricted: true
        rotation:
          mode: PasswordDriven
```

```bash
kubectl apply -f controlplane.yaml
```

::: tip No `gateway` block here
The ControlPlane exposes a **curated subset** of the Keystone knobs and does not
surface a `gateway` field, so the projected Keystone CR has no Gateway
attachment and is reachable in-cluster only. Verification below therefore uses
`kubectl port-forward` rather than the `nip.io` Gateway endpoint. If you also
want external access, apply a separate `Keystone`-level Gateway route or use the
per-service [Quick Start](./quick-start.md).
:::

## Step 7 — Watch the chain reconcile

The aggregate `Ready` becomes `True` only after all five sub-conditions are
`True`, gated in dependency order:

```
InfrastructureReady → KeystoneReady → KORCReady → AdminCredentialReady → CatalogReady
```

Watch the ControlPlane and its sub-conditions:

```bash
kubectl get controlplane -n openstack -w
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

Inspect the projected children:

```bash
# Infrastructure + Keystone projected by the ControlPlane
kubectl get mariadb,memcached,keystone -n openstack

# K-ORC resources the operator drives (group openstack.k-orc.cloud)
kubectl get applicationcredentials,services,endpoints.openstack.k-orc.cloud -n openstack
```

If `AdminCredentialReady` lingers on `WaitingForCloudsYaml`, force the
`k-orc-clouds-yaml` ExternalSecret to sync (Step 2 does not wait on it
explicitly, so it may otherwise refresh only on its hourly interval):

```bash
kubectl annotate externalsecret/k-orc-clouds-yaml -n openstack \
  force-sync="$(date +%s)" --overwrite
kubectl get externalsecret k-orc-clouds-yaml -n openstack
```

Then wait for the aggregate condition:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=10m
```

## Step 8 — Verify

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
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
```

**The identity catalog** — the catalog sub-reconciler registers the Keystone
identity `Service` and public `Endpoint` as K-ORC CRs:

```bash
kubectl get services.openstack.k-orc.cloud,endpoints.openstack.k-orc.cloud -n openstack
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.catalogReady}{"\n"}'
```

**An authenticated token** — port-forward the projected Keystone Service and
issue a token with the admin password:

```bash
# In a separate terminal:
kubectl port-forward svc/controlplane-keystone -n openstack 5000:5000

# Then, in your shell:
export OS_AUTH_URL=http://localhost:5000/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack token issue
```

> The projected Keystone Service is named `{controlplane-name}-keystone` —
> `controlplane-keystone` for the CR above. Confirm with
> `kubectl get svc -n openstack`.

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
