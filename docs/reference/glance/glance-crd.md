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
| `dbPurge` | [`*DBPurgeSpec`](#dbpurgespec) | no | Recurring database purge that hard-deletes rows Glance only ever soft-deletes. The operator resolves the effective settings at reconcile time, so a nil block and an empty struct behave alike: 30-day retention, daily at `1 0 * * *`, task rows only, not suspended |
| `staging` | [`*StagingSpec`](#stagingspec) | no | Bounds the node-local scratch space an image import may consume. The operator resolves the effective limit at reconcile time, so a nil block, an empty struct, and a set block leaving `sizeLimit` unset all behave alike: `10Gi` on each of the two scratch volumes. `unbounded: true` opts out of the bound entirely |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute on port 9292; requires `hostname` and `parentRef.name` |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 9292 from the listed sources; egress auto-derived (DNS, database, cache, and the attached backends' S3 hosts). At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets |
| `logging` | `*LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false` |
| `secretStoreRef` | `*SecretStoreRefSpec` | no | Selects the External Secrets store the operator resolves `SecretsReady` against — `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used. Normally projected from the owning ControlPlane |
| `policyOverrides` | `*PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders a `policy.yaml` and wires `oslo_policy.policy_file` |
| `middleware` | `[]MiddlewareSpec` | no | WSGI middleware filters injected into the `api-paste.ini` pipeline |
| `plugins` | `[]PluginSpec` | no | Service plugins/drivers, modeled as a list-map keyed by `configSection` so duplicate sections are rejected structurally |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for configuration not covered by explicit fields — the escape hatch for options with no dedicated knob of their own. The render-time merge follows `plugins < operator defaults < extraConfig` (each stage merged key-wise), so user values win over both. Overrides of operator-owned keys are honored but reported (report-only) via the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event — except `[keystone_authtoken] password`, which the validating webhook rejects at admission so the env-injected service password never leaks into the namespace-readable ConfigMap, and the six `[import_filtering_opts]` keys, which it rejects so the `web-download` URI filter stays reachable only through [`spec.importFiltering`](#importfilteringspec) and its admission gates. Option names are validated at admission against a per-release option catalog embedded in the operator (release derived from `spec.openStackRelease`; a release with no embedded catalog skips the check with an admission warning). Sections declared by `spec.plugins`, operator-owned keys, and the reserved store sections `os_glance_staging_store` and `os_glance_tasks_store` are exempt. An unknown section or option is rejected with the section and key named, so an arbitrary backend-named section is refused; backend options belong to the [`GlanceBackend`](./glance-backend-crd.md) CR. A deprecated-but-accepted option is admitted with a warning naming its replacement. Values are checked for one thing only — a newline or carriage return in a section name, key, or value is rejected, because the rendered INI writes each verbatim and a newline would inject arbitrary config lines past the ownership and catalog gates. Every rule here is webhook-only with no CEL backstop |

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

### DBPurgeSpec

Glance never hard-deletes on its own: deleting an image only flips its row to
deleted, and every image import leaves a task row behind, so both grow for the
lifetime of the deployment. This block tunes the recurring `{name}-db-purge`
CronJob that reclaims that backlog.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `retentionDays` | `*int32` (Minimum=1) | no | `30` | How long a soft-deleted row survives before the purge hard-deletes it — the `--age_in_days` argument both commands take. The one-day floor keeps the purge from racing rows an in-flight import just wrote. Lowering it applies retroactively at the next firing, so the validating webhook warns on a reduction |
| `schedule` | `string` | no | `"1 0 * * *"` | The cron expression the purge CronJob runs on. Checked by the validating webhook rather than a CRD pattern, so a descriptor such as `@daily` is accepted alongside standard 5-field expressions. Also the only lever over purge throughput (see `max_rows` below) |
| `purgeImagesTable` | `*bool` | no | `false` | Chains the second `glance-manage db purge_images_table` pass, hard-deleting the images rows themselves and freeing their UUIDs for reuse |
| `suspend` | `bool` | no | `false` | Pauses the CronJob without deleting it, matching `TrustFlushSpec.suspend` on Keystone. `DBPurgeReady` stays `True` — a paused purge is a posture, not a failure — but reports reason `DBPurgeSuspended` and a message naming the growing backlog, since nothing else surfaces a purge that stopped |

The operator resolves the knobs at reconcile time, the same contract
[ImportFilteringSpec](#importfilteringspec) follows: a field left unset keeps
tracking the operator default across upgrades instead of freezing today's
value into the stored CR, so a nil `spec.dbPurge` resolves exactly like an
empty struct. The defaulting webhook leaves the block untouched for that
reason. The task sweep is scheduled on every Glance either way, since an
unbounded soft-delete backlog is an outage waiting to happen; what varies is
the window, the frequency, whether the images pass is chained on, and whether
the CronJob is running at all.

The CronJob renders as:

| CronJob field | Value |
| --- | --- |
| `metadata.name` | `{name}-db-purge` |
| `metadata.labels` | `commonLabels` (same as Deployment) |
| `spec.schedule` | resolved `dbPurge.schedule` |
| `spec.suspend` | resolved `dbPurge.suspend` |
| `spec.concurrencyPolicy` | `Forbid` |
| `spec.jobTemplate.spec.activeDeadlineSeconds` | `3600` |
| `spec.jobTemplate.spec.template.spec.restartPolicy` | `OnFailure` |
| Container name | `db-purge` |
| Container image | `spec.image` reference |
| Container command | `glance-manage --config-dir /etc/glance/glance-api.conf.d/ db purge --age_in_days <retentionDays> --max_rows 1000`, with ` && glance-manage --config-dir /etc/glance/glance-api.conf.d/ db purge_images_table --age_in_days <retentionDays> --max_rows 1000` appended when `purgeImagesTable` is true |
| Container securityContext | `RestrictedSecurityContext()` (PSS Restricted) |
| Container volumes | The rendered config ConfigMap, plus the db-tls keypair when `spec.database.tls` is enabled |
| `ownerReferences` | Points to the Glance CR (controller: true) |

`db purge` sweeps the tasks table and the image child tables in one pass but
leaves the images table itself alone, and that omission is a security property:
while the row survives, its UUID stays reserved, so a deleted image's ID can
never be claimed by a different image (OSSN-0075). Because Glance's v2
`image-create` accepts a client-supplied `id`, hard-deleting the row lets any
project member claim the freed UUID — and every Heat template, Terraform state,
or server `image_ref` still pinning it would then resolve to that image. The
second `db purge_images_table` pass is therefore opt-in via `purgeImagesTable`,
for deployments whose policy denies client-supplied image IDs. When enabled it
runs after the first, so the child rows referencing an image are already gone by
the time that image's own row is purged.

`max_rows` (1000) bounds each per-table delete transaction so a long-neglected
backlog cannot stall a Galera cluster for the length of one write-set. It is
operator-owned rather than a CRD field, set well above Glance's own built-in
default of 100 rows. The cap applies per invocation, so the schedule sets the
throughput: on the default daily schedule the purge drains at most 1000 rows
per table per day. That keeps up only while the deployment soft-deletes fewer
rows per table than one run removes — a cloud importing more than 1000 images a
day grows its tasks table faster than the purge drains it, and a brownfield
backlog takes one run per batch to work off. `schedule` is the lever for both:
an hourly purge multiplies the daily ceiling by 24.

`concurrencyPolicy: Forbid` keeps a run that outlasts its interval from being
overtaken by the next firing — two purges deleting the same rows contend, which
is what the per-transaction cap exists to avoid. `activeDeadlineSeconds` bounds
how long a single run may stay active, so a purge wedged on a database lock or
an unschedulable pod reaches a terminal `Failed` state instead of staying
active indefinitely with `DBPurgeReady` still reporting a healthy schedule.

One run can still fail visibly. With `purgeImagesTable` enabled, an image row
past its retention window can have a younger child row referencing it, an edge
case caused by unusual timestamp skew, and `db purge_images_table` then exits
non-zero with a `DBReferenceError`. That flips `DBPurgeReady` to `False`
(reason `DBPurgeJobFailed`) and raises a Warning event; a later run converges
once the child row also crosses the retention window.

Rolling this operator onto a deployment that has never been purged applies the
retention window retroactively on the first firing. To stage that, set
`suspend: true`, raise `retentionDays` to cover the deployment's full history,
un-suspend, and step the retention down while watching
`glance_operator_db_purge_total`.

### StagingSpec

Every image import lands on local disk before the data reaches the backing
store. The reserved `os_glance_staging_store` takes the uploaded or downloaded
image, the reserved `os_glance_tasks_store` the async task's working copy, and
both are `emptyDir` volumes on the node filesystem. This block caps them.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `sizeLimit` | `*resource.Quantity` | no | `10Gi` | The `emptyDir.sizeLimit` stamped on each of the two scratch volumes, `staging` and `tasks-work`. Must be at least `1Mi` |
| `unbounded` | `bool` | no | `false` | Renders both scratch volumes with no `sizeLimit` at all, the shape a `Glance` had before this block existed. Mutually exclusive with `sizeLimit` |

The operator resolves the value at reconcile time, the contract
[ImportFilteringSpec](#importfilteringspec) and [DBPurgeSpec](#dbpurgespec)
already follow: a nil `spec.staging`, an empty struct, and a set block whose
`sizeLimit` is unset all resolve to the operator default of `10Gi`, and the
defaulting webhook writes nothing back into the CR. An unset field therefore
keeps tracking that default across upgrades instead of freezing today's value
into the stored spec.

One value covers two volumes, so one glance-api pod is expected to occupy at
most twice the configured limit: 20Gi at the default.

Two caveats decide how much node disk that number really claims. The bound is an
eviction threshold, not a filesystem quota — the kubelet evaluates it on its
periodic local-storage housekeeping pass (~10 s), so a writer overshoots by
whatever it appends within one pass, and a transfer shorter than one pass can
finish before the kubelet ever looks. And the bound is not a scheduling
reservation: the operator derives no `resources.requests.ephemeral-storage` from
it, so co-scheduled replicas each get their own budget and the sum is bounded by
the node's disk rather than by this field. Size nodes against `replicas × 2 ×
sizeLimit` plus that overshoot. To make the scheduler account for it, add
`ephemeral-storage` to `spec.deployment.resources.requests` — and spell out the
CPU and memory values in the same block, because a `resources` block that is
present at all suppresses the operator's resource defaults.

The kubelet enforces the bound; Glance never sees it and keeps writing. Once an
`emptyDir` grows past its `sizeLimit`, local-storage eviction evicts the
glance-api pod. The in-flight import dies with the pod and its image never
reaches `active`. The Deployment then replaces the pod and the API recovers
without operator or human intervention.

Within one pod the bound is a single shared budget, not per-import accounting.
Concurrent imports draw from it together, nothing admits or queues an import
against the remaining space, and breaching it evicts the pod rather than the
offending import — so every in-flight transfer on that replica dies with it,
including well-behaved ones started by other projects. Size the bound for the
concurrency the deployment expects, and run more than one replica so an eviction
does not take the API with it.

`web-download` imports are what make the bound matter. Glance fetches the whole
remote image onto the staging volume first and moves it into the backing store
afterwards, so a remote image larger than the limit trips the bound long before
it reaches the store. [ImportFilteringSpec](#importfilteringspec) governs which
URIs may be fetched at all; this block governs how much disk one fetch may
consume.

`unbounded: true` is the opt-out, and the only way to say "no bound" — the
operator then renders both `emptyDir`s without a `sizeLimit`, exactly as it did
before this block existed. It is a separate field rather than a sentinel
`sizeLimit` value so the `1Mi` floor keeps rejecting the sub-byte quantities it
exists to catch, and setting both is rejected at admission. Prefer a `sizeLimit`
large enough for the deployment: an unbounded volume puts nothing between one
runaway `web-download` import and the node's disk, and the pods evicted when
that disk fills are ranked across the whole node, not only Glance's.

::: warning The bound applies retroactively on operator upgrade
Before this block existed the two scratch volumes were unbounded. Upgrading the
operator stamps the resolved `10Gi` onto every existing `Deployment`, which
rolls the glance-api pods — killing any in-flight import — and then evicts the
pod on an import that used to succeed above `10Gi`.

Neither field can be set ahead of that upgrade. The block ships with the same
chart version that introduces the behaviour, so against the older CRD a patch
naming `spec.staging` is pruned as an unknown field: the request succeeds and
stores nothing. The schema has to land before the value can, which leaves one
working sequence — quiesce imports, upgrade, then set `sizeLimit` or
`unbounded`, rolling the pods a second time.
:::

The limit lives in the pod template, not in `glance-api.conf`. Changing it
re-stamps `emptyDir.sizeLimit` on both volumes and rolls the Deployment, leaving
the rendered config untouched. The effective bound is readable from the live
Deployment, and is empty when `unbounded` is set:

```bash
kubectl get deploy glance -n openstack \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="staging")].emptyDir.sizeLimit}{"\n"}'
```

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

The defaulting webhook leaves `spec.dbPurge` untouched for the same reason:
its fields are resolved at reconcile time (see
[DBPurgeSpec](#dbpurgespec)), not written back into the CR. The validating
webhook still checks the block as defense in depth: `retentionDays` mirrors
the CRD's `Minimum=1` marker, and `schedule` is checked against the cron
grammar, the one rule with no CRD-schema counterpart, since a regex
expressive enough to accept a descriptor like `@daily` alongside standard
5-field expressions would also accept invalid ones. On update it also warns
when `retentionDays` is reduced: the shorter window applies retroactively at
the next firing and the rows it removes do not come back.

`spec.staging` is left alone by the defaulting webhook for the same reason. Its
two rules split: `unbounded` and `sizeLimit` are mutually exclusive, enforced by
a CEL rule on the block with the validating webhook repeating it, because
`unbounded` is a plain boolean the schema can reason about. The floor has no
schema counterpart at all: `sizeLimit` must be at least
`1Mi`, and the validating webhook is the only gate on it. A `resource.Quantity`
renders as `x-kubernetes-int-or-string` in the CRD schema, a type that carries
no `Minimum` marker, and Kubernetes core validation rejects only a *negative*
`emptyDir.sizeLimit` — so everything from `0` up to the sub-byte milli suffix
reaches the kubelet verbatim. The floor is `1Mi` rather than mere positivity
because the schema pattern accepts `100m`, a tenth of a byte and the most common
typo for `100Mi`: admitted, it would evict the glance-api pod on its first
staged byte and the replacement on the next import, with nothing in the CR
status naming the cause. A `ControlPlane` calls the same exported validator on
`spec.services.glance.staging`, so both CRs admit the same values.

The validating webhook additionally bounds `metadata.name` at 43 characters.
The `{name}-db-purge` CronJob is the child object with the tightest name
budget — Kubernetes caps a CronJob name at 52 characters, because its
controller appends an 11-character timestamp suffix to every Job it spawns —
so a longer Glance would name a CronJob the API server refuses to create. The
bound applies on create only: `metadata.name` is immutable, so on update it
could only reject a CR an earlier operator version already admitted —
including the finalizer-removal update that completes its deletion. Such a
grandfathered CR still reconciles: the operator collapses the overflowing tail
of the name onto a content-stable hash, naming its CronJob
`{truncated}-{hash}-db-purge` instead of `{name}-db-purge`.

The same bound reaches one level up. A `ControlPlane` projects its Glance
child as `{controlplane}-glance`, so a ControlPlane declaring
`spec.services.glance` is capped at 36 characters; its own validating webhook
rejects a longer one at create time, and on update when that update is what
enables Glance.

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
length, port range, and 64-item cap of each list, the `dbPurge.retentionDays`
floor and the `dbPurge.schedule` cron grammar,
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
| DB-purge CronJob | `{name}-db-purge` | `glance-manage db purge`, plus `db purge_images_table` when opted in |

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
