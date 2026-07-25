---
title: Glance CRD
quadrant: operator
---

# Glance CRD

`glances.glance.openstack.c5c3.io/v1alpha1`, kind `Glance`. The CRD is
generated from `operators/glance/api/v1alpha1/glance_types.go`; the validating
and defaulting webhooks live in
`operators/glance/api/v1alpha1/glance_webhook.go`. The Helm chart ships a synced
copy (`make sync-crds` / `make verify-crd-sync`).

One Glance CR describes the Image API server: its OpenStack release, container
image, database and cache connections, and the Keystone integration. Image
stores are **not** part of this spec — they attach out-of-band through
[`GlanceBackend`](./glance-backend-crd.md) CRs.

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `openStackRelease` | `string` | yes | The OpenStack release the operator deploys and drives; pattern `^\d{4}\.[12]$` (the `YYYY.N` cadence, `N` ∈ {1,2}). Governs the API launch mode (eventlet below `2026.1`, uWSGI from `2026.1`) and install/upgrade schema tracking. Kept separate from the image tag so digest-pinned images still resolve a schema and launch mode |
| `deployment` | `DeploymentSpec` | no | Shared pod-level knobs: `replicas` (default 3), `resources` (defaults: 512Mi request / 1Gi limit memory, 100m/500m CPU), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `image` | `ImageSpec` | yes | Container image; exactly one of `tag` or `digest` (shared CEL rule, re-checked by the webhook) |
| `database` | `DatabaseSpec` | yes | MariaDB connection. Exactly one of `clusterRef` (managed) or `host` (brownfield); `credentialsMode` (`Static` \| `Dynamic`, where `Dynamic` requires `clusterRef`), `secretRef`, and optional `tls`. Mutual-exclusivity and the Dynamic-requires-clusterRef rule are inherited from `commonv1.DatabaseSpec` |
| `cache` | `CacheSpec` | yes | Memcached backing the Glance image cache. Exactly one of `clusterRef` (managed) or `servers` (brownfield) |
| `keystoneEndpoint` | `string` | yes | The Keystone auth URL rendered as `[keystone_authtoken] auth_url`; must match `^https?://` and parse with a host. Consumed server-side on every request, so it must be reachable from inside the cluster — use the cluster-local Service URL, not an externally routable address |
| `keystonePublicEndpoint` | `string` | no | The browser/client-facing Keystone base URL rendered as `[keystone_authtoken] www_authenticate_uri` (the address a 401 points unauthenticated clients at); must match `^https?://` when set. When empty the operator falls back to `keystoneEndpoint` at render time (see `EffectiveKeystonePublicEndpoint`), correct only when the internal and public Keystone URLs coincide |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | yes | The Keystone service account Glance authenticates as, and the Secret holding its password |
| `region` | `string` | no | The Keystone region (`[keystone_authtoken] region_name`); when empty the option is omitted and Glance uses the catalog's default region |
| `apiServer` | [`*APIServerSpec`](#apiserverspec) | no | Release-conditional API-process tuning; when nil the operator uses hardcoded defaults for the active launch mode |
| `importFiltering` | [`*ImportFilteringSpec`](#importfilteringspec) | no | URI filtering for `web-download` image imports. The operator resolves the effective lists at render time, so a nil block and an empty struct behave alike: HTTPS on port 443, plus a literal host denylist |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute on port 9292; requires `hostname` and `parentRef.name` |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 9292 from the listed sources; egress auto-derived (DNS, database, cache, and the attached backends' S3 hosts). At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets |
| `logging` | `*LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false` |
| `secretStoreRef` | `*SecretStoreRefSpec` | no | Selects the External Secrets store the operator resolves `SecretsReady` against — `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used. Normally projected from the owning ControlPlane |
| `policyOverrides` | `*PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders a `policy.yaml` and wires `oslo_policy.policy_file` |
| `middleware` | `[]MiddlewareSpec` | no | WSGI middleware filters injected into the `api-paste.ini` pipeline |
| `plugins` | `[]PluginSpec` | no | Service plugins/drivers, modeled as a list-map keyed by `configSection` so duplicate sections are rejected structurally |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for configuration not covered by explicit fields — the escape hatch for import/staging tuning. The render-time merge follows `plugins < operator defaults < extraConfig` (each stage merged key-wise), so user values win over both. Overrides of operator-owned keys are honored but reported (report-only) via the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event — except `[keystone_authtoken] password`, which the validating webhook rejects at admission so the env-injected service password never leaks into the namespace-readable ConfigMap, and the six `[import_filtering_opts]` keys, which it rejects so the `web-download` URI filter stays reachable only through [`spec.importFiltering`](#importfilteringspec) and its admission gates. Option names are validated at admission against a per-release option catalog embedded in the operator (release derived from `spec.openStackRelease`; a release with no embedded catalog skips the check with an admission warning). Sections declared by `spec.plugins`, operator-owned keys, and the reserved store sections `os_glance_staging_store` and `os_glance_tasks_store` are exempt. An unknown section or option is rejected with the section and key named, so an arbitrary backend-named section is refused; backend options belong to the [`GlanceBackend`](./glance-backend-crd.md) CR. A deprecated-but-accepted option is admitted with a warning naming its replacement. Values are checked for one thing only — a newline or carriage return in a section name, key, or value is rejected, because the rendered INI writes each verbatim and a newline would inject arbitrary config lines past the ownership and catalog gates. Every rule here is webhook-only with no CEL backstop |

### ServiceUserSpec

The name and domain fields are webhook-defaulted, so a minimal CR need only
supply the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `glance` | Keystone username (`[keystone_authtoken] username`) |
| `projectName` | `string` | no | `service` | The project the service user scopes to (`project_name`) |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in (`user_domain_name`) |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in (`project_domain_name`) |
| `secretRef` | `SecretRefSpec` | yes | `key` → `password` | The Secret holding the service-user password; the password is injected as the `OS_KEYSTONE_AUTHTOKEN__PASSWORD` env var, never rendered into config |

### APIServerSpec

Tunes the API server process. Which field takes effect depends on
`spec.openStackRelease`, and the validating webhook emits an admission
**warning** (not a rejection) on an inert combination — both knobs are legal in
either mode, the operator simply ignores the inert one.

| Field | Type | Required | Effective when | Description |
| --- | --- | --- | --- | --- |
| `uwsgi` | [`*UWSGISpec`](#uwsgispec) | no | release ≥ `2026.1` (uWSGI launch mode) | uWSGI application-server parameters; inert below `2026.1` |
| `workers` | `*int32` (Minimum=1) | no | release < `2026.1` (eventlet launch mode) | The eventlet API worker count, rendered as `[DEFAULT] workers`; inert from `2026.1` |

### UWSGISpec

A cross-field CEL rule mirrors the webhook: `httpKeepAliveTimeout` may only be
set when `httpKeepAlive` is true (otherwise the `--http-keepalive-timeout` flag
is never emitted).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `processes` | `int32` (Minimum=1) | no | `2` | uWSGI worker-process count (`--processes`) |
| `threads` | `int32` (Minimum=1) | no | `1` | Threads per worker (`--threads`) |
| `httpKeepAlive` | `*bool` | no | `true` | Enables `--http-keepalive`. A nil-preserving pointer: nil restores the default, an explicit `false` is honored verbatim |
| `harakiri` | `*int32` (Minimum=1) | no | — | Per-request worker lifetime cap (`--harakiri`, seconds). Omitted entirely when nil (no hidden default). The webhook requires `harakiri < terminationGracePeriodSeconds − preStopSleepSeconds` |
| `httpKeepAliveTimeout` | `*int32` (Minimum=1) | no | — | Idle timeout of keep-alive connections (`--http-keepalive-timeout`, seconds). Omitted when nil; zero is rejected to avoid the unbounded interpretation |

### ImportFilteringSpec

Constrains the URIs the `web-download` image-import method may fetch from,
rendered as the `[import_filtering_opts]` group. Each attribute (scheme, host,
port) has an allow-list and a deny-list, and Glance only evaluates one of the
two: a non-empty allow-list makes it ignore the matching deny-list. Configuring
both halves of a pair would silently drop the deny-list, so three CEL rules
reject that combination, and the validating webhook mirrors them.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `allowedSchemes` | `[]string` (MaxItems=64, items ∈ {`http`, `https`}) | no | Schemes `web-download` may fetch from (`allowed_schemes`). Glance's own default is `[http, https]`; the operator narrows an unset field to `[https]` |
| `disallowedSchemes` | `[]string` (MaxItems=64, items ∈ {`http`, `https`}) | no | Schemes `web-download` must refuse (`disallowed_schemes`). Ignored by Glance whenever `allowedSchemes` is non-empty |
| `allowedHosts` | `[]string` (MaxItems=64, item length 1–253) | no | Hosts `web-download` may fetch from (`allowed_hosts`). Non-empty pins imports to a known mirror and refuses every other host |
| `disallowedHosts` | `[]string` (MaxItems=64, item length 1–253) | no | Hosts `web-download` must refuse (`disallowed_hosts`). An unset field resolves to the operator denylist below, covering loopback, the link-local metadata address, and the in-cluster API server; entries set here are unioned onto that denylist rather than replacing it. Ignored by Glance whenever `allowedHosts` is non-empty |
| `allowedPorts` | `[]int32` (MaxItems=64, 1–65535) | no | Ports `web-download` may connect to (`allowed_ports`). Glance's own default is `[80, 443]`; the operator narrows an unset field to `[443]` |
| `disallowedPorts` | `[]int32` (MaxItems=64, 1–65535) | no | Ports `web-download` must refuse (`disallowed_ports`). Ignored by Glance whenever `allowedPorts` is non-empty |

The defaults are resolved at render time; the mutating webhook never writes them
into the CR, so a field left unset keeps tracking the operator default across
upgrades. Only a nil field is defaulted. An explicit empty list is honored as
empty, which is how a deployment opts out of a default restriction, and a nil
`spec.importFiltering` resolves the same way an empty struct does.

| Unset field | Resolves to |
| --- | --- |
| `allowedSchemes` | `https` |
| `disallowedSchemes` | empty |
| `allowedHosts` | empty |
| `disallowedHosts` | `localhost`, `127.0.0.1`, `0.0.0.0`, `::1`, `169.254.169.254`, `kubernetes`, `kubernetes.default`, `kubernetes.default.svc`, `kubernetes.default.svc.cluster.local`; empty while `allowedHosts` is non-empty |
| `allowedPorts` | `443` |
| `disallowedPorts` | empty |

Resolution never widens one default because of a value set on another field: a
deny-list entry keeps the sibling allow-list default, and `disallowedHosts`
entries are **unioned** onto the denylist above rather than replacing it, so
tightening the filter cannot un-deny loopback, the metadata address, or the API
server. The one carve-out left is `disallowedHosts` while `allowedHosts` is
non-empty: Glance ignores the host denylist then, and the allow-list is the
stricter half, so rendering the baseline would misreport what applies.

That makes a deny-list on `disallowedSchemes` or `disallowedPorts` **inert on
its own**, because Glance evaluates a deny-list only while the matching
allow-list is empty and the operator default keeps it populated. The shape is
admitted — it is not wrong, only ineffective — so the validating webhook raises
an admission warning naming the inert list. To make one authoritative, empty the
sibling allow-list explicitly — the pair stays within the mutual-exclusivity
rule, which is keyed on non-empty lists:

```yaml
spec:
  importFiltering:
    allowedSchemes: []          # opt out of the https pin …
    disallowedSchemes: [http]   # … so this deny-list is what applies
```

All six keys are rendered on every reconcile, empty values included. Glance's
own `allowed_schemes` and `allowed_ports` defaults are non-empty, so a key left
out of the file would mean "Glance's permissive default", not "unrestricted by
this operator".

Host matching is literal string membership. Glance supports no CIDR ranges, no
wildcards, and no DNS-resolution-based blocking, so every host has to be spelled
out the way an import URI would spell it. The default list therefore carries all
four spellings of the API server Service a pod's `resolv.conf` search list
resolves, but it still covers the names it lists and nothing else: a
trailing-dot FQDN, a raw ClusterIP, or an in-cluster HTTPS service on port 443
whose name is absent stays reachable from a `web-download` import. The
scheme-and-port pin is the control that holds there; where the residual reach
matters, pin `allowedHosts` to the mirrors imports are supposed to use.

A mirror that only serves plaintext HTTP needs both the scheme and the port
widened, since the two are filtered independently:

```yaml
spec:
  importFiltering:
    allowedSchemes: [http, https]
    allowedPorts: [80, 443]
```

`disallowedHosts` stays unset there, so the operator's host denylist keeps
applying — but it is now the only control left, and it matches literally.
Widening either allow-list past the operator default therefore raises an
admission **warning**: the scheme-and-port pin is what keeps an import off the
link-local metadata endpoint (which answers on `http`/80), and once it is gone
an alternate spelling of that address — an integer or IPv4-mapped form, a raw
ClusterIP, a trailing-dot FQDN — is not covered by any denylist entry. Contain
the widened reach instead of relying on the denylist: pin `allowedHosts` to the
mirrors imports are supposed to use, or restrict the API pods' egress with
`spec.networkPolicy.additionalEgress`.

::: warning Upgrading an operator that did not render this group
Before the operator rendered `[import_filtering_opts]`, `web-download` ran under
Glance's own defaults: schemes `http` and `https`, ports 80 and 443, no host
denylist. Every Glance CR has `spec.importFiltering` unset, so the first reconcile
after the operator upgrade narrows the filter to HTTPS on port 443 — no CR edit
triggers it. The change rewrites the content-hashed config ConfigMap and rolls the
Deployment, and from then on an import from an `http://` mirror or a non-443 port
fails immediately with a synchronous `400`: it is a filter rejection, not a
network or mirror fault.

Before upgrading, check which URIs the deployment imports from. If any is
plaintext or off port 443, apply the widening block above in the same change. The
effective filter is always readable from the rendered config:

```bash
kubectl get cm -n openstack "$(kubectl get deploy glance -n openstack \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')" \
  -o 'jsonpath={.data.glance-api\.conf}' | grep -A6 import_filtering_opts
```
:::

All six keys are registered as operator-owned and rejected at admission when set
through `spec.extraConfig` — unlike most owned keys, which are honoured and merely
reported. `spec.importFiltering` expresses every one of them, so an `extraConfig`
override adds no reach; it only skips the exclusivity rules, the host INI guard,
and the warning that flags a loosened filter, leaving an audit that reads
`spec.importFiltering` with the restrictive default while the rendered config says
otherwise. Any other key under `[import_filtering_opts]` is rejected too: the
section appears in no option catalog.

Changing the filter rewrites the content-hashed config ConfigMap, and the new
hash rolls the Deployment. When `spec.networkPolicy` is set on the CR, a
`web-download` import additionally needs a matching
`spec.networkPolicy.additionalEgress` rule: the auto-derived egress covers DNS,
the database, the cache, and the backends' S3 hosts, and nothing beyond them.

### Defaulting and validation

The mutating webhook applies the shared `DeploymentSpec`/`LoggingSpec` defaults
— with one glance-specific deviation: an unset `spec.deployment.resources` is
filled with 512Mi memory request / 1Gi memory limit (CPU keeps the shared
100m/500m), because the glance-api container carries the boto3-weighted S3
store driver and overruns the shared 512Mi baseline under concurrent image
traffic. It also materializes the `PyMemcacheCache` cache backend, fills the
`ServiceUserSpec` identity defaults (`glance` / `service` / `Default` /
`Default`, `secretRef.key` → `password`), and — only when
`spec.apiServer.uwsgi` is present — the uWSGI sub-field defaults (`processes`
2, `threads` 1, `httpKeepAlive` true). It leaves `spec.importFiltering`
untouched: those lists are resolved when the config is rendered (see
[ImportFilteringSpec](#importfilteringspec)), so materializing them here would
freeze today's values into the stored CR.

The validating webhook accumulates every violation into one admission response,
reusing the shared validators: replicas floor, image tag/digest XOR, the
database mutual-exclusivity and Dynamic-requires-clusterRef rules, cache
mutual-exclusivity, secret-store-ref shape, both Keystone endpoint URL shapes,
logging enums (including the per-logger-level map, which has no schema-layer
counterpart), the graceful-termination cross-field arithmetic
(`preStopSleepSeconds < terminationGracePeriodSeconds`, and `harakiri` strictly
inside the drain window), the `Recreate`-vs-`rollingUpdate` sanity check,
autoscaling bounds (including the implicit `minReplicas` default from
`deployment.replicas`), network-policy ingress, gateway hostname/parentRef, the
three `importFiltering` allow/deny pairings together with the scheme enum, host
length, port range, and 64-item cap of each list,
resource requests-vs-limits, PriorityClass existence, topology-spread selectors
(matching the `glance` / instance labels), and the `extraConfig` guards (a
preserve-unknown-fields map CEL cannot constrain): empty section/key names, a
newline or carriage return in any section name, key, or value, and the rejected
overrides of `[keystone_authtoken] password` (owned via
`spec.serviceUser.secretRef`) and the six `[import_filtering_opts]` keys (owned
via `spec.importFiltering`).

Two `importFiltering` rules have no schema-layer counterpart. A host carrying a
newline or carriage return is rejected outright: the host lists are the only
free-form strings in the block and are joined verbatim into
`[import_filtering_opts]`, where a newline injects arbitrary config lines and
smuggles a whole section past the `extraConfig` ownership and catalog gates,
which inspect map structure and never look inside a value. And the two
posture problems the schema cannot express are reported as admission warnings
rather than rejections, because both are legal deployment choices: a deny-list
that Glance will never evaluate, and an allow-list widened past the operator
default. Both are also raised on `spec.services.glance.importFiltering` of a
ControlPlane, which is where most deployments author the filter.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./glance-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The Glance API URL: `https://{gateway.hostname}/` when a gateway is set, otherwise the cluster-local Service URL |
| `installedRelease` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after the upgrade completes (or after the first `db sync` on a fresh install) |
| `targetRelease` | The `spec.openStackRelease` being upgraded to during an active release transition. Set when the upgrade initiates, cleared on completion or abort; empty in steady state |
| `upgradePhase` | The current expand-migrate-contract phase during an active release upgrade (`Expanding`, `Migrating`, `RollingUpdate`, `Contracting`); empty when no upgrade is in flight |

::: info
A release transition (a `spec.openStackRelease` bump with the image in lockstep)
walks the shared expand-migrate-contract phase machine, tracked through
`upgradePhase`/`targetRelease`. See the
[Glance Upgrade Flow](./glance-upgrade-flow.md) for the phases, condition
reasons, events, and abort semantics.
:::

## Sub-Resource Naming Convention

Operator-managed sub-resources (Deployment, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute) use the bare CR name with no
suffix, matching the keystone convention. A Glance CR named `glance` in the
`openstack` namespace is therefore reachable in-cluster at
`glance.openstack.svc.cluster.local:9292` — the Service DNS name is the CR name.

The content-addressed and derived resources are the exceptions:

| Resource | Name | Notes |
| --- | --- | --- |
| Config ConfigMap | `{name}-config-<hash>` | Immutable, content-hashed `glance-api.conf`; 3 historical retained |
| Backends Secret | `{name}-backends-<hash>` | Immutable, content-hashed `backends.conf` (the aggregated store sections); 3 historical retained |
| DB-connection Secret | `{name}-db-connection` | Derived pymysql DSN, stable name |
| DB-sync Job | `{name}-db-sync` | `glance-manage db sync` |

## Example

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: glance
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/glance
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-mariadb
    database: glance
    secretRef:
      name: glance-db
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: glance-keystone
```
