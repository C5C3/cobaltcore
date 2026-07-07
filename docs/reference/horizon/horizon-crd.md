---
title: Horizon CRD
quadrant: operator
---

# Horizon CRD

`horizons.horizon.openstack.c5c3.io/v1alpha1`, kind `Horizon`. The CRD is
generated from `operators/horizon/api/v1alpha1/horizon_types.go`; the Helm
chart ships a synced copy (`make sync-crds` / `make verify-crd-sync`).

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | Shared pod-level knobs: `replicas` (default 3), `resources` (Burstable defaults), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `image` | `ImageSpec` | yes | Container image; exactly one of `tag` or `digest` (shared CEL rule) |
| `cache` | `CacheSpec` | yes | Memcached backing the Django cache. Exactly one of `clusterRef` (managed) or `servers` (brownfield); `backend` is a Django cache backend path, defaulted to `django.core.cache.backends.memcached.PyMemcacheCache` |
| `keystoneEndpoint` | `string` | yes | The Keystone endpoint URL (`OPENSTACK_KEYSTONE_URL`); must match `^https?://` and parse with a host. Consumed server-side by the dashboard pods, so it must be reachable from inside the cluster — for a colocated control plane use the cluster-local Service URL, not an externally routable address |
| `secretKeyRef` | `SecretRefSpec` | yes | Secret holding the Django `SECRET_KEY`; `key` defaults to `secret-key`. Injected as the `HORIZON_SECRET_KEY` env var, never into the ConfigMap |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute on port 8080; requires `hostname` and `parentRef.name` |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 8080 from the listed sources; egress auto-derived (DNS, the Keystone endpoint port, cache ports). At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets |
| `logging` | `*LoggingSpec` | no | Django `LOGGING` dictConfig derivation: root `level`, `debug`, `perLoggerLevels`. Defaulted to `text`/`INFO`/`debug: false` |
| `extraConfig` | `map[string]JSON` | no | Free-form Django settings rendered after the operator defaults (user values win). `SECRET_KEY` is rejected by the webhook |

### Defaulting and validation

The mutating webhook applies the shared `DeploymentSpec`/`LoggingSpec`
defaults, materializes the PyMemcacheCache backend, and defaults
`secretKeyRef.key`. The validating webhook accumulates every violation:
replicas floor, image tag/digest XOR, cache mutual exclusivity, the
`keystoneEndpoint` URL shape, gateway hostname/parentRef, network-policy
ingress, autoscaling bounds (including the implicit `minReplicas` default
from `deployment.replicas`), logging enums, graceful-termination cross-field
arithmetic, topology-spread selectors, and PriorityClass existence.

A CEL rule over `extraConfig` keys is not expressible (the API server cannot
build CEL type information for preserve-unknown-fields map values), so the
empty-key and `SECRET_KEY` guards are webhook-only.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./horizon-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The dashboard URL: `https://{gateway.hostname}/` when a gateway is set, otherwise the cluster-local Service URL |

## Sub-Resource Naming Convention

Operator-managed sub-resources (Deployment, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute) use the bare CR name with
no suffix, matching the keystone convention. A Horizon CR named `horizon` in
the `openstack` namespace is therefore reachable in-cluster at
`horizon.openstack.svc.cluster.local:8080` — the Service DNS name is the CR
name. The immutable settings ConfigMap is the one derived name:
`{name}-config-<content-hash>`.

## Example

```yaml
apiVersion: horizon.openstack.c5c3.io/v1alpha1
kind: Horizon
metadata:
  name: horizon
  namespace: openstack
spec:
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/horizon
    tag: "2025.2"
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  secretKeyRef:
    name: horizon-secret-key
  gateway:
    parentRef:
      name: openstack-gw
      namespace: envoy-gateway-system
    hostname: horizon.127-0-0-1.nip.io
```
