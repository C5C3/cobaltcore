# kind development environment

These overlays configure a small local development/CI stack. The metal-stack
lab also reuses several of these definitions, so changes here can affect both
environments. Reusable deployment packages are listed in the
[base catalog](../bases/README.md).

From the repository root:

```bash
make deploy-infra
```

For the operator-managed ControlPlane workflow:

```bash
WITH_CONTROLPLANE=true make deploy-infra
```

Follow the [ControlPlane quick start](../../docs/quick-start-controlplane.md) for
host ports, prerequisites, the ControlPlane CR and database onboarding.
`WITH_CONTROLPLANE=true` prepares the operators; it does not itself request the
bundled CR. `WITH_CONTROLPLANE_CR=true` requests that additional step. The
`CONTROLPLANE_OPERATORS=external` mode lets CI supply its own operator builds.

## Entry points

| Directory | Consumer and order |
| --- | --- |
| [`base/`](base/) | `deploy-infra`, after the Flux APIs are ready; sources, releases, development sizing and gateway/UI resources |
| [`infrastructure/`](infrastructure/) | `deploy-infra`, after operator CRDs; certificates, service instances, secret shims and the proving OpenBao instance |
| [`controlplane/`](controlplane/) | The quick start or `WITH_CONTROLPLANE_CR=true`, after the operator/bootstrap prerequisites |
| `chaos-mesh/`, `dizzy/`, `nfs/` | Optional addons selected by the matching `WITH_*` flags |
| [`prometheus-managed/`](prometheus-managed/) | `WITH_PROMETHEUS=true` after the platform base owns the monitoring Namespace |
| [`prometheus/`](prometheus/) | Existing direct-consumption root, including its own monitoring Namespace |
| `metrics-server/`, `vpa/` | Optional kind sizing/measurement support; VPA also requires metrics-server |
| [`messaging/`](messaging/) | Optional standalone RabbitMQ after infrastructure; used in both kind and external-cluster mode |
| [`fake-compute/`](fake-compute/) | Manually selected Nova testing fixture |
| [`hypervisor-operator-fixtures/`](hypervisor-operator-fixtures/) | Manually selected K-ORC fixtures after the ControlPlane is ready |

The script preserves the boundary between static infrastructure and
ControlPlane-owned resources. Do not independently apply the full static
`infrastructure/` overlay over a ControlPlane-managed installation: the script
filters the database/cache and selected standalone secret shims for that mode.

The shared OpenBao server runs standalone here while retaining TLS/mTLS. Its
configuration deliberately omits Kubernetes service registration. The separate
proving OpenBao instance has its own lifecycle and receives its unseal key from
the shared backend; see [OpenBao roles](../openbao/README.md).

The composed kind and lab flows use the Prometheus variant without a Namespace
so the platform base retains that resource's labels and ownership. The original
`prometheus/` output remains available to direct consumers. Both variants need
the existing Prometheus chart source and generated dashboard input.

## Preview and generated inputs

These commands render YAML without deploying it:

```bash
kubectl kustomize deploy/kind/base
kubectl kustomize deploy/kind/infrastructure
```

Prometheus and Dizzy need generated dashboard files before a direct render.
Their `WITH_*` deployment paths prepare them automatically. To prepare only
those inputs, use `make stage-prometheus-dashboard` and
`hack/dizzy.sh stage-dashboards`, respectively. The lab variants inherit these
inputs as well. Keep the default Kustomize load restrictions.

See the [deployment reference](../../docs/reference/infrastructure/e2e-deployment.md)
for the complete flag and phase contract, and the
[infrastructure reference](../../docs/reference/infrastructure/infrastructure-manifests.md)
for individual addons.
