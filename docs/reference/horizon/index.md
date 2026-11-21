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
  no database sub-reconcilers at all. A Memcached that hangs or restarts only
  lowers the hit-rate too, because the operator renders 1 s pymemcache
  timeouts and `ignore_exc` into `CACHES["default"]["OPTIONS"]`, which turn
  every failed cache call into a miss (#1560). DB-backed sessions are
  revisited only if signed cookies plus Memcached prove insufficient. The
  whole session, the scoped token and its service catalog included, travels
  in the `sessionid` cookie (see [Session size limits](#session-size-limits)).
- **Cache keys: scoped per image.** The operator derives
  `CACHES["default"]["KEY_PREFIX"]` from `spec.image`: `horizon-` plus the
  first 12 hex characters of the SHA-256 of the image reference. Horizon
  keeps its Angular template-cache preload in the Django fragment cache, and
  django-compressor hashes that fragment against the offline manifest built
  into each image. A fragment written by another release has no entry in
  that manifest, so every page render raises `OfflineGenerationError` and
  the dashboard answers HTTP 500, the login page included. With the prefix,
  Horizon CRs on different images never read each other's entries. An image
  change starts with a cold cache, and two CRs on one image share a warm one
  ([#1305](https://github.com/C5C3/cobaltcore/issues/1305)). The
  `tests/e2e/horizon/release-upgrade/` suite moves a Horizon from 2026.1 to
  2026.2 and asserts that the prefix the Deployment mounts changes with the
  image, and that the login page renders on both sides of the hop.
- **Requests to the services: bounded at 30 seconds.** The operator renders
  `OPENSTACK_REQUEST_TIMEOUT = 30`, which horizon patch 0002 passes as
  `timeout` to every keystoneauth session and service client the dashboard
  builds, the login included. The sessions pass no retries, so a call to a
  service that stops answering ends after 30 seconds, inside the gateway's
  60-second route, and the user sees Horizon's own error message instead of a
  gateway 503 or 504. Without the bound the call waits out the kernel's TCP
  retransmission limit, about 15 minutes, or glanceclient's own 600 seconds,
  and holds the uWSGI process that serves it. The cost is that a call that is
  legitimately silent for longer fails, which includes the wait for Glance's
  answer at the end of an image upload through Horizon.
  `spec.extraConfig` raises the bound (`OPENSTACK_REQUEST_TIMEOUT: 60`), which
  is honored and reported. Django ignores a setting no code reads, so the key
  takes effect only on an image that carries the patch (see
  [Container Images](../ci-cd/container-images.md#horizon)). The operator
  upgrade that brings the setting renders a new ConfigMap, so the dashboard
  rolls once.
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
- **WSGI server: uWSGI.** Pinned in the horizon lock under `nix/locks/` from
  the base pins of `nix/base-requirements.txt` and operationally identical to the keystone
  deployment; uWSGI loads `openstack_dashboard.wsgi` directly. No per-CR uWSGI
  knobs in v1. uWSGI runs with `--http-keepalive`, so a client or a proxy can
  send several requests on one connection. uWSGI's HTTP router closes a
  connection that has been idle for 60 seconds. uWSGI runs with
  `--buffer-size 65535` and accepts that many bytes of request line and
  headers, where its default is 4096 bytes (see
  [Session size limits](#session-size-limits)). The operator upgrade that
  brings the flag changes the pod template, so the dashboard rolls once.
  SIGTERM starts the same graceful shutdown as in the API operators' uWSGI:
  the requests in flight finish, then uWSGI exits (see the
  [Keystone shutdown sequence](../keystone/keystone-crd.md#shutdown-sequence)).
- **Login-page probes.** The readiness and startup probes GET `/auth/login/`
  with a fixed `Host: localhost` header, which satisfies Django's
  `ALLOWED_HOSTS` check without allow-listing the pod IP; rendering the page
  exercises Django's URL routing, the templates and the offline-compression
  manifest without a live Keystone. The liveness probe only opens a TCP
  connection to port 8080. The startup probe allows 300 seconds (30 probes
  10 seconds apart, each with an 8-second timeout) before the liveness probe
  takes over, so a cold start slowed by a CPU limit set on the container or
  a contended node does not restart the container.
- **The `horizon===` constraint pin is stripped.** Unlike keystone, horizon
  is pinned in `upper-constraints.txt`; `overrides/<release>/constraints.txt`
  removes the pin so the source install builds the ref from
  `source-refs.yaml`.
- **Two Warning events, one optional finalizer.** The operator records no
  Normal lifecycle events; conditions carry the state contract. It records two
  Warning events: `ExtraConfigOwnedKeyOverride` when `spec.extraConfig`
  overrides an operator-owned setting (see the
  [extraConfig ownership guard](./horizon-crd.md#extraconfig-ownership-guard)),
  and `RemoteChildrenAbandoned` when deletion begins while the target cluster
  a CR named no longer resolves. A CR without `spec.targetClusterRef` carries
  no finalizer: every owned resource is namespace-scoped and
  garbage-collected via ownerReferences. A CR with it carries the
  `openstack.c5c3.io/remote-children` finalizer, which sweeps the children on
  the target cluster.

## Session size limits

The signed-cookie store puts the whole session into the `sessionid` cookie,
and the browser sends it back with every logged-in request. Two limits bound
its size: uWSGI on the server side and the browser on the client side.

uWSGI accepts a request line and headers up to its `--buffer-size`. The
operator sets 65535, the largest value uWSGI accepts. Under the default of
4096 bytes, uWSGI logs `invalid request block size: <n> (max 4096)...skip`
for a larger request and closes the connection without a response, and a
gateway in front answers 503. The login page and the login carry no session
cookie, so they keep working while every page after the login fails. With
the flag, uWSGI serves a request with a 60000-byte cookie. It still drops one
with a 64000-byte cookie, and logs nothing for it.

The operator cannot raise the browser's limit. A browser keeps about 4096
bytes per cookie
([RFC 6265, section 6.1](https://www.rfc-editor.org/rfc/rfc6265#section-6.1))
and discards a larger `sessionid` cookie. The next request arrives without a
session, the user is on the login page again, and Horizon logs nothing.

The cookie grows with the service catalog. The table gives the value of the
`sessionid` cookie a login sets, measured in the 2026.1 image (Horizon
25.7.0, Django 4.2.28) for
[#1841](https://github.com/C5C3/cobaltcore/issues/1841). The catalog is
synthetic, with random 32-hex ids and two endpoints per service as the
ControlPlane registers them; a catalog with longer endpoint URLs gives a
larger cookie.

| Services with two endpoints | Cookie value, bytes |
| --- | --- |
| 8 | 2216 |
| 12 | 2754 |
| 16 | 3163 |
| 20 | 3569 |
| 24 | 3957 |
| 26 | 4162 |

A service without endpoints adds about 46 bytes, a service with two
endpoints 100 to 135 bytes. Keystone lists every enabled service in the
catalog, also one without endpoints.

Horizon logs this line at ERROR, from the logger `horizon.middleware.base`,
to the dashboard container's stderr through the console handler of the
`LOGGING` setting the operator renders:

```text
Total Cookie size for user_id: <id> is <n>B >= 4093B. You need to configure file-based or database-backed sessions instead of cookie-based sessions: https://docs.openstack.org/horizon/latest/admin/sessions.html
```

It does so while the signed-cookie store is in use, for an authenticated
request that is not AJAX, once the names and values of the cookies the
request sends sum to 4093 bytes (`SESSION_COOKIE_MAX_SIZE`). The line needs
a request that reaches Django: under the default buffer, uWSGI drops such a
request before Django sees it. Since Horizon reads the cookies a
request sends, the line warns between 4093 bytes and the browser's limit and
stays silent once the browser has discarded the cookie. When it appears,
delete the services the cloud does not need from the Keystone catalog, the
ones without endpoints first.

`tests/e2e/horizon/basic-deployment/` and its 2026.2 twin send the login page
a 6000-byte and a 60000-byte cookie and expect HTTP 200 for both. Link 2c and
Link 8e of
[`full-controlplane-keystone`](../testing/controlplane-e2e-tests.md#full-controlplane-keystone)
request `/project/api_access/` after a login, expect HTTP 200, and fail once
the cookies that request sends sum to 4093 bytes.

## Owned resources

The figure under
[Owned Resources](../keystone/keystone-reconciler.md#owned-resources) of the
Keystone operator draws the baseline this list follows: serving objects, config
and Secrets, Jobs and CronJobs.

For a Horizon CR named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The uWSGI dashboard pods (port 8080) |
| Service | `{name}` | ClusterIP in front of the dashboard pods |
| PodDisruptionBudget | `{name}` | The budget [`spec.podDisruptionBudget`](../keystone/keystone-crd.md#poddisruptionbudgetspec) sets; unset, `minAvailable: 1` (or `maxUnavailable: 1` at a lower replica bound of one, which is the HPA's `minReplicas` while `spec.autoscaling` is set) |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `local_settings.py` |
| HorizontalPodAutoscaler | `{name}` | Only when `spec.autoscaling` is set |
| NetworkPolicy | `{name}` | Only when `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only when `spec.gateway` is set |

## Reference pages

- [Horizon CRD](./horizon-crd.md) — the full `spec`/`status` contract
- [Reconciler Architecture](./horizon-reconciler.md) — the sub-reconciler
  chain, conditions, and requeue semantics
