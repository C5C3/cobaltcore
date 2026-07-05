---
title: Horizon Operator
quadrant: operator
---

# Horizon Operator

The Horizon operator deploys and manages the OpenStack Dashboard as a
Kubernetes-native workload. It is the second service operator built on the
shared scaffolding the Keystone operator established (`internal/common`, the
`operator-library` Helm chart, the parameterized operator image) and proves
the onboarding path is repeatable.

Horizon's profile is deliberately the thin one among the service operators: a
stateless Django/WSGI web application with no message bus, no service-catalog
endpoints of its own, and none of Keystone's db-sync/fernet/bootstrap/upgrade
machinery. What it needs instead: Memcached for the Django cache, the Keystone
public endpoint, a shared Django `SECRET_KEY`, pre-built static assets, and —
uniquely among the services — HTTP ingress by default.

## Design decisions

The v1 operator resolves the onboarding decisions as follows:

- **Sessions: signed cookies, no database.** `SESSION_ENGINE` is Django's
  `signed_cookies` backend and Memcached only backs the Django cache
  (`CACHES`, `PyMemcacheCache`). Losing Memcached degrades cache hit-rate; it
  neither logs users out nor flips any condition. The operator therefore has
  no database sub-reconcilers at all. DB-backed sessions are revisited only
  if signed cookies plus Memcached prove insufficient.
- **Keystone endpoint: a plain URL field.** `spec.keystoneEndpoint` keeps the
  operator decoupled from the keystone-operator. The c5c3 ControlPlane
  operator derives the value top-down from its Keystone child's naming
  convention (never from the child's status — no machine consumer reads
  status endpoints).
- **`SECRET_KEY`: ESO-managed Secret, env-var-injected.** The key is required
  for multi-replica signed-cookie consistency, synced from OpenBao via an
  ExternalSecret (mirroring the keystone credential pattern), injected into
  the pods as the `HORIZON_SECRET_KEY` environment variable, and never
  rendered into the settings ConfigMap. The validating webhook rejects a
  `SECRET_KEY` entry in `spec.extraConfig`.
- **Static assets: built at image-build time.** `collectstatic --noinput` and
  `compress --force` run in the image build against a throwaway settings
  file; uWSGI serves the result via `--static-map`. No runtime init
  container. See [Container Images](../ci-cd/container-images.md#horizon).
- **WSGI server: uWSGI.** Already pinned in the shared venv-builder image and
  operationally identical to the keystone deployment; uWSGI loads
  `openstack_dashboard.wsgi` directly. No per-CR uWSGI knobs in v1.
- **The `horizon===` constraint pin is stripped.** Unlike keystone, horizon
  is pinned in `upper-constraints.txt`; `overrides/<release>/constraints.txt`
  removes the pin so the source install builds the ref from
  `source-refs.yaml`.
- **No events, no finalizer in v1.** The operator emits no custom Kubernetes
  events (conditions carry the full state contract), and every owned resource
  is namespace-scoped and garbage-collected via ownerReferences, so deletion
  needs no finalizer pass. An events reference page is added when the first
  event lands.

## Owned resources

For a Horizon CR named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The uWSGI dashboard pods (port 8080) |
| Service | `{name}` | ClusterIP in front of the dashboard pods |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` (or `maxUnavailable: 1` at a single replica) |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `local_settings.py` |
| HorizontalPodAutoscaler | `{name}` | Only when `spec.autoscaling` is set |
| NetworkPolicy | `{name}` | Only when `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only when `spec.gateway` is set |

## Reference pages

- [Horizon CRD](./horizon-crd.md) — the full `spec`/`status` contract
- [Reconciler Architecture](./horizon-reconciler.md) — the sub-reconciler
  chain, conditions, and requeue semantics
