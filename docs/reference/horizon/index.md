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

- **Sessions: signed cookies by default, no database.** Without
  `spec.session`, `SESSION_ENGINE` is Django's `signed_cookies` backend.
  `spec.session.backend: Cache` is the opt-in that moves the sessions into
  Memcached (see [Cache sessions](#cache-sessions)). With signed cookies, the
  operator's default settings and an image that carries horizon patch 0004,
  no request calls Memcached
  ([#1847](https://github.com/C5C3/cobaltcore/issues/1847)). Losing Memcached
  then neither logs users out nor flips any condition, and the operator has
  no database sub-reconcilers at all. `CACHES["default"]` (`PyMemcacheCache`)
  stays on Memcached for what a deployment adds: a cache `SESSION_ENGINE`
  through `spec.extraConfig`, `SIMULTANEOUS_SESSIONS: disconnect`, a plugin.
  The operator renders 1 s pymemcache timeouts and `ignore_exc` into
  `CACHES["default"]["OPTIONS"]`, which bound those calls and turn every
  failed one into a miss (#1560). An image without patch 0004 keeps the
  session cookie of every logged-in user in `default`: Horizon's
  `SimultaneousSessionsMiddleware` stores the session key under
  `user_pk_<pk>_restrict` on every request for `SESSION_TIMEOUT` (3600 s),
  and the session key of a signed-cookie session is the cookie. The cookie is
  signed and decodes without `SECRET_KEY`, so any client of that Memcached
  can read the Keystone token in it. `SIMULTANEOUS_SESSIONS: disconnect` keeps
  doing so on a patched image. DB-backed sessions are revisited only if
  signed cookies plus Memcached prove insufficient. The whole session, the
  scoped token and its service catalog included, travels in the `sessionid`
  cookie (see [Session size limits](#session-size-limits)).
- **Cache keys: fragments per process, the rest scoped per image.** Horizon
  keeps its Angular template-cache preload, about 265 KB, in the Django
  fragment cache of every page. The operator renders a
  `template_fragments` alias on Django's `LocMemCache`, which the
  `{% cache %}` tag prefers over `default`, so each uWSGI process keeps the
  fragment itself
  ([#1847](https://github.com/C5C3/cobaltcore/issues/1847)). Kept in
  `default`, the fragment costs one of three Memcached calls per request: a
  Memcached delayed by 100 ms raised the p50 of the login page from 23 ms to
  921 ms
  ([C5C3/playground#165](https://github.com/C5C3/playground/issues/165)).
  django-compressor hashes the fragment against the offline manifest built
  into each image. A fragment written by another release has no entry in
  that manifest, so a shared fragment makes every page render raise
  `OfflineGenerationError` and the dashboard answer HTTP 500, the login page
  included ([#1305](https://github.com/C5C3/cobaltcore/issues/1305)). A
  fragment no other process reads cannot carry that failure. The operator
  derives `CACHES["default"]["KEY_PREFIX"]` from `spec.image`:
  `horizon-` plus the first 12 hex characters of the SHA-256 of the image
  reference. With the prefix, Horizon CRs on different images never read
  each other's entries in `default`. An image change starts with a cold
  cache, and two CRs on one image share a warm one. The
  `tests/e2e/horizon/release-upgrade/` suite moves a Horizon from 2026.1 to
  2026.2 and asserts that the prefix the Deployment mounts changes with the
  image, and that the login page renders on both sides of the hop. The
  `sessions` alias of [cache sessions](#cache-sessions) has a prefix of its
  own that does not follow the image.

  The alias changes the ConfigMap of every Horizon, so the operator upgrade
  that brings it rolls each dashboard once. `SECRET_KEY` and the `sessions`
  prefix do not change, so signed-cookie and cache sessions are kept. The
  new operator with an image without horizon patch 0004 leaves two Memcached
  calls per request, those of `SimultaneousSessionsMiddleware`. An older
  operator with a patched image leaves one, the fragment's. Neither
  combination fails. Each uWSGI process, two per pod, keeps one fragment per
  theme it rendered, about 265 KB for the theme `default`, and renders it
  once after its start in about 22 ms. Fragments older pods left in
  Memcached stay until their 30-day expiry or an eviction, and nothing reads
  them. The `user_pk_<pk>_restrict` entries that pods on an image without
  patch 0004 wrote stay as well, and nothing deletes them: after the move to
  a patched image, each still holds the session cookie, Keystone token
  included, of a user who was active before the move, until
  `SESSION_TIMEOUT` (3600 s) has passed since that user's last request to an
  unpatched pod. On a signed-cookie Horizon with a dedicated Memcached, a
  restart of that Memcached after the rollout clears them at once.
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
  uWSGI runs with `--max-fd 65536` like the API operators' uWSGI, so its
  HTTP router starts within the pod's memory limit under any open-files
  limit the node hands out (see the
  [Keystone open-files limit](../keystone/keystone-crd.md#open-files-limit)).
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
session, and the user is on the login page again. With horizon patch 0003 the
login page then names the cause and the dashboard logs a line, as described
below.

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
request sends, the line warns between 4093 bytes and the browser's limit.
Once the browser has discarded the cookie, the request carries none, and this
line cannot appear.

Horizon patch 0003 ([Container Images](../ci-cd/container-images.md#horizon))
adds a check on the response side. It reads the `sessionid` cookie a response
sets and fires once the cookie's name and value reach 4093 bytes. It logs this
line at ERROR, from the logger `horizon.middleware.session_cookie_size`, to
stderr through the same console handler:

```text
Session cookie for user_id: <id> is <n>B >= 4093B. A browser discards a cookie of this size, so the session is lost. You need to configure server-side sessions instead of cookie-based sessions: https://docs.openstack.org/horizon/latest/admin/sessions.html
```

The same response sets the `logout_reason` and `logout_status` cookies. The
login page then shows
`Your session is too large for a browser cookie and the login cannot complete. Contact your administrator.`
in its red alert box. The message is English in every locale. The response is
otherwise unchanged, so a client without a cookie limit stays logged in and
gets the line for every response that sets the cookie. A deployment that sets
`WEBSSO_DEFAULT_REDIRECT` through `spec.extraConfig` never renders the login
page, so only the log line appears there.

When either line appears, delete the services the cloud does not need from
the Keystone catalog, the ones without endpoints first.

`tests/e2e/horizon/basic-deployment/` and its 2026.2 twin send the login page
a 6000-byte and a 60000-byte cookie and expect HTTP 200 for both. Link 2c and
Link 8e of
[`full-controlplane-keystone`](../testing/controlplane-e2e-tests.md#full-controlplane-keystone)
request `/project/api_access/` after a login, expect HTTP 200, and fail once
the cookies that request sends sum to 4093 bytes.

When the catalog cannot shrink, [cache sessions](#cache-sessions) take the
session out of the cookie.

## Cache sessions

`spec.session.backend: Cache` keeps the session in the Memcached of
`spec.cache`, which must use the `PyMemcacheCache` backend (see
[SessionSpec](./horizon-crd.md#sessionspec)). On the ControlPlane the block
is `spec.services.horizon.session`, and the c5c3-operator projects it onto
the Horizon child. The operator renders three settings:

```python
SESSION_ENGINE = "django.contrib.sessions.backends.cache"
SESSION_CACHE_ALIAS = "sessions"
CACHES = {
    "default": {...},
    "sessions": {...},  # BACKEND, LOCATION and OPTIONS of "default"
    "template_fragments": {...},  # per-process LocMemCache
}
```

The `sessions` alias reaches the same servers as `default`, with the same
1-second pymemcache timeouts and `ignore_exc`. Its `KEY_PREFIX` is
`horizon-sessions-` plus the first 12 hex characters of the SHA-256 of
`<namespace>/<name>` of the Horizon CR: `openstack/horizon-release-upgrade`
gives `horizon-sessions-bf5308222bb5`. The prefix does not follow the image,
so an image change keeps the sessions, which the
`tests/e2e/horizon/release-upgrade/` suite checks by writing a session on the
2026.1 pod and reading it on the 2026.2 pod. Two dashboards on one Memcached
have different prefixes and do not accept each other's session keys. The hash
keeps the Memcached key under its 250-byte limit whatever the length of the
name.

With cache sessions the cookie carries only a 32-character session key, so
neither of the [session size limits](#session-size-limits) bounds the
session, and Horizon's `Total Cookie size` check does not run. Every logged-in request reads its
session from Memcached, a call the 1-second timeouts bound.

The failure behaviour was measured on 2026-11-21 with
`ghcr.io/c5c3/horizon:2026.1` (Horizon 25.7.0, Django 4.2.28, pymemcache
4.0.0), one Memcached 1.6 server and the `OPTIONS` the operator renders,
through Django's cache `SessionStore`. HTTP status codes were not measured.

| Memcached | Reading a session | Creating a session (a login) | Saving a loaded session |
| --- | --- | --- | --- |
| stalled (container paused) | empty session after 1.00 s: the user is logged out | `RuntimeError: Unable to create a new session key. It is likely that the cache is unavailable.` after 2.1 s | `UpdateError` |
| stopped | empty session at once | the same `RuntimeError` after 1.15 s | `UpdateError` |

Without `ignore_exc` the reads and saves behave the same, and the create
raises `TimeoutError` (stalled) or `socket.gaierror` (stopped), so the
`sessions` alias keeps `ignore_exc` like `default`. Django answers an unhandled exception with HTTP 500: a login during
a Memcached outage fails with an error page.

Memcached does not replicate. A lost or restarted Memcached pod, or an
eviction under memory pressure, logs out the users whose sessions it held.
A change of the server list (a Memcached replica change, the first per-pod
list the Memcached publishes, a switch to or from
`dedicatedBackingServices.cache`) rehashes the session keys and logs out the
users whose keys moved. While the list is a Service name in front of several
pods (the Memcached has published no `status.serverList` yet, or a brownfield
`spec.cache.servers` entry names such a Service), each request reaches one of
those pods at random, and a request on a pod that does not hold the session
logs the user out. Brownfield `spec.cache.servers` for cache sessions lists
every Memcached instance under a name of its own.

Switching the store to `Cache` or back renders a new ConfigMap and rolls the
dashboard once. The old and the new pods do not accept each other's cookies,
so the switch logs every user out, and again each time a request moves
between an old and a new pod, until the rollout completes. A nil block and
`backend: SignedCookies` render the same module.

With cache sessions, Memcached holds the session keys and the Keystone
tokens of every logged-in user in clear. A client that reaches its port
11211 can list the keys, read the tokens and take over the sessions. With
signed cookies, an image without horizon patch 0004 puts the same tokens
into Memcached under `user_pk_<pk>_restrict` (see
[Design decisions](#design-decisions)). The c5c3-operator sets no NetworkPolicy, SASL or TLS on the Memcached CRs it
creates, the shared one and a dedicated one alike, so restricting access to
them is left to the cluster's own NetworkPolicies.
`services.horizon.dedicatedBackingServices.cache` gives the dashboard a
Memcached of its own (see the
[ControlPlane CRD](../c5c3/controlplane-crd.md#servicehorizonspec)), which
keeps the sessions off the Memcached the other services use but is reachable
the same way.

Multi-domain login is the second reason to choose cache sessions. For a user
who holds a role on the domain they log in to and on no project,
`openstack_auth` uses the domain-scoped token as the scoped one. Under signed
cookies it leaves that token out of the session and logs this line at ERROR:

```text
Using signed cookies as SESSION_ENGINE with OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT is enabled. This disables the ability to perform identity operations due to cookie size constraints.
```

With cache sessions the session keeps the token under `domain_token`. Step 7
of [`e2e-controlplane-sso`](../testing/controlplane-e2e-tests.md#federated-controlplane)
logs in such a user through the domain dropdown, checks that the session
holds `domain_token`, and checks that no dashboard pod logs the line.

`spec.extraConfig.SESSION_ENGINE` without the typed block is honored and
reported as an
[owned-key override](./horizon-crd.md#extraconfig-ownership-guard). It keeps
the sessions in the `default` alias, whose image-scoped prefix loses them on
every image change.

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
