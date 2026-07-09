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
| `websso` | `*WebSSOSpec` | no | Federated single-sign-on choices on the login page. When nil the operator renders no `WEBSSO_*` settings and the dashboard offers local credentials only. The c5c3 ControlPlane projects this from the federation backends attached to its Keystone child |
| `multiDomain` | `*MultiDomainSpec` | no | Multi-domain login: the domain field (or dropdown) on the login form, needed when users live in domains other than the default one — typically LDAP-backed. When nil the operator renders no `OPENSTACK_KEYSTONE_MULTIDOMAIN_*` settings |

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

## WebSSOSpec

Maps onto Horizon's `WEBSSO_*` Django settings. Rendered by the settings
renderer, so operators never hand-write Python literals into `extraConfig`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `enabled` | `bool` | no | `false` | Turns the SSO selector on the login page on (`WEBSSO_ENABLED`). When false the remaining fields are inert |
| `choices` | `[]WebSSOChoice` | no | — | Ordered entries of the "Authenticate using" dropdown (`WEBSSO_CHOICES`); order is preserved verbatim. Required (non-empty) when `enabled` is true. The defaulting webhook **prepends** the local-credentials fallback when no choice carries the id `credentials`, which is what stops enabling SSO from locking out non-federated accounts. Max 17 counts the list *after* that prepend — the 16 federated entries `idpMapping` bounds, plus the fallback — so a list that already declares `credentials` may hold 17 entries and one that does not is bounded at 16. Submitting a 17th without the fallback is rejected by the **defaulting** webhook, because mutating admission runs before schema validation and a prepend would otherwise be rejected on a count you never wrote |
| `idpMapping` | `map[string]WebSSOIDPTarget` | no | — | Maps a choice id onto the Keystone identity provider and federation protocol (`WEBSSO_IDP_MAPPING`). A choice with no mapping entry is a local login. Every key must name a declared choice. Max 16 |
| `initialChoice` | `string` | no | `credentials` | Preselects one of `choices` by id (`WEBSSO_INITIAL_CHOICE`). Must name a declared choice |
| `keystoneURL` | `string` | no | `""` | The **browser-facing** Keystone base URL the SSO redirect is built from (`WEBSSO_KEYSTONE_URL`); matches `^https?://`. It exists because `spec.keystoneEndpoint` is by contract the cluster-local Service URL, consumed server-side — redirecting a browser there would fail to resolve. When empty the setting is omitted and Horizon falls back to `spec.keystoneEndpoint` |

### WebSSOChoice

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | `string` | yes | Submitted as the form's `auth_type` value and referenced by `idpMapping` / `initialChoice`. Matches `^[A-Za-z0-9_.-]+$` (it round-trips through a URL query string) |
| `label` | `string` | yes | Human-readable text rendered for this choice |

### WebSSOIDPTarget

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `identityProvider` | `string` | yes | The Keystone identity-provider id (the backend's effective `identityProviderName`) |
| `protocol` | `string` | yes | The Keystone federation protocol id (e.g. `openid`) |

### Operator-pinned WebSSO settings

Two settings are rendered by the operator and are **not** configurable through
`spec.websso`:

- `WEBSSO_USE_HTTP_REFERER = False`. It defaults to `True` upstream, which makes
  `openstack_auth` validate the returned token against the Keystone URL derived
  from the browser's `Referer` — i.e. the external gateway URL, resolved
  server-side from inside the pod, where it does not reach Keystone. With
  `False` it validates against `OPENSTACK_KEYSTONE_URL` instead.
- `SECURE_PROXY_SSL_HEADER = ["HTTP_X_FORWARDED_PROTO", "https"]`, rendered when
  `spec.gateway` is set. A Gateway terminates TLS and forwards plain HTTP, so
  without it Django reports `http://` and the origin it sends Keystone never
  matches an `https://` `trusted_dashboard`. Gating on the Gateway is what makes
  trusting the header safe: Envoy overwrites `X-Forwarded-Proto` on ingress.

## MultiDomainSpec

Configures the login form's domain handling. Horizon only renders the domain
dropdown when `domainDropdown` is true **and** `domainChoices` is non-empty;
with `enabled` alone the form shows a free-text domain field.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `enabled` | `bool` | no | `false` | Turns on multi-domain login (`OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT`) |
| `defaultDomain` | `string` | no | `Default` | The domain assumed for users who supply none (`OPENSTACK_KEYSTONE_DEFAULT_DOMAIN`). Materialized by the defaulting webhook |
| `domainDropdown` | `bool` | no | `false` | Replaces the free-text domain field with a select populated from `domainChoices` (`OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN`). Requires `enabled` and a non-empty `domainChoices` |
| `domainChoices` | `[]DomainChoice` | no | — | Ordered entries of the domain dropdown (`OPENSTACK_KEYSTONE_DOMAIN_CHOICES`). Rendered only when `domainDropdown` is true. Max 32 |

### DomainChoice

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | yes | The Keystone domain name submitted with the login form |
| `label` | `string` | yes | Human-readable text rendered for this domain |

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
