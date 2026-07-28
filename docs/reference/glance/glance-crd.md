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
| `importPlugins` | [`*ImportPluginsSpec`](#importpluginsspec) | no | Selects the image-import plugins Glance runs, rendered as `[image_import_opts] image_import_plugins` plus the section each enabled plugin reads. Presence of a sub-block enables that plugin, nil enables none; the rendered order is fixed (`image_decompression`, `image_conversion`, `inject_image_metadata`) and is not an input. Every default resolves at render time, so an unset field keeps tracking the operator default |
| `dbPurge` | [`*DBPurgeSpec`](#dbpurgespec) | no | Recurring database purge that hard-deletes rows Glance only ever soft-deletes. The operator resolves the effective settings at reconcile time, so a nil block and an empty struct behave alike: 30-day retention, daily at `1 0 * * *`, task rows only, not suspended |
| `staging` | [`*StagingSpec`](#stagingspec) | no | Bounds the node-local scratch space an image import may consume. The operator resolves the effective limit at reconcile time, so a nil block, an empty struct, and a set block leaving `sizeLimit` unset all behave alike: `10Gi` on each of the two scratch volumes. `unbounded: true` opts out of the bound entirely |
| `imageCache` | [`*ImageCacheSpec`](#imagecachespec) | no | Turns on the per-replica local image cache: presence of the block enables it, nil disables it. `sizeLimit` bounds the cache `emptyDir` (default `10Gi`, floor `1Mi`) and `maintenanceInterval` sets the pruner/cleaner cadence (default `5m`, floor `1m`). Both resolve at render time, so an unset field keeps tracking the operator default |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute on port 9292; requires `hostname` and `parentRef.name` |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 9292 from the listed sources; egress auto-derived (DNS, database, cache, and the attached backends' S3 hosts). At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets |
| `logging` | `*LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false` |
| `secretStoreRef` | `*SecretStoreRefSpec` | no | Selects the External Secrets store the operator resolves `SecretsReady` against — `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used. Normally projected from the owning ControlPlane |
| `policyOverrides` | `*PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders a `policy.yaml` and wires `oslo_policy.policy_file` |
| `middleware` | `[]MiddlewareSpec` | no | WSGI middleware filters injected into the `api-paste.ini` pipeline |
| `plugins` | `[]PluginSpec` | no | Service plugins/drivers, modeled as a list-map keyed by `configSection` so duplicate sections are rejected structurally |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for configuration not covered by explicit fields — the escape hatch for options with no dedicated knob of their own. The render-time merge follows `plugins < operator defaults < extraConfig` (each stage merged key-wise), so user values win over both. Overrides of operator-owned keys are honored but reported (report-only) via the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event — except `[keystone_authtoken] password`, which the validating webhook rejects at admission so the env-injected service password never leaks into the namespace-readable ConfigMap, the six `[import_filtering_opts]` keys, which it rejects so the `web-download` URI filter stays reachable only through [`spec.importFiltering`](#importfilteringspec) and its admission gates, and the three `[DEFAULT] image_cache_*` keys, which it rejects because each of them ends in an evicted glance-api pod or in database rows nothing reclaims — see [ImageCacheSpec](#imagecachespec). The four image-import plugin keys are rejected on the same footing, so the plugin pipeline stays reachable only through [`spec.importPlugins`](#importpluginsspec) and the order, enum, and property-name rules it carries. Option names are validated at admission against a per-release option catalog embedded in the operator (release derived from `spec.openStackRelease`; a release with no embedded catalog skips the check with an admission warning). Sections declared by `spec.plugins`, operator-owned keys, and the reserved store sections `os_glance_staging_store` and `os_glance_tasks_store` are exempt. An unknown section or option is rejected with the section and key named, so an arbitrary backend-named section is refused; backend options belong to the [`GlanceBackend`](./glance-backend-crd.md) CR. A deprecated-but-accepted option is admitted with a warning naming its replacement. Values are checked for one thing only — a newline or carriage return in a section name, key, or value is rejected, because the rendered INI writes each verbatim and a newline would inject arbitrary config lines past the ownership and catalog gates. Every rule here is webhook-only with no CEL backstop |

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

### ImportPluginsSpec

Glance can put an imported image through a chain of plugins before the data
reaches the backing store: unpack a compressed one, rewrite it into a single
disk format, stamp image properties onto it. A plugin runs only while
`[image_import_opts] image_import_plugins` names it, and this block is that
naming. Presence of a sub-block adds its plugin to the list, nil leaves it out,
the same opt-in convention [ImageCacheSpec](#imagecachespec) follows.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `decompression` | `*ImportDecompressionSpec` | no | Enables `image_decompression`, which unpacks a compressed image after staging so the later plugins and the store see the disk image instead of the archive. The struct carries no fields: upstream registers no options for the plugin, so its presence is the whole configuration. Setting it requires [`spec.staging`](#stagingspec) to answer for the scratch bound — either `sizeLimit` or `unbounded` — because the plugin's expansion is unbounded and the operator default was sized against the largest download |
| `conversion` | `*ImportConversionSpec` | no | Enables `image_conversion`, which rewrites the staged image into one target disk format |
| `injectMetadata` | `*ImportInjectMetadataSpec` | no | Enables `inject_image_metadata`, which stamps a fixed set of image properties onto every image it applies to |

`conversion` carries one field:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `outputFormat` | `string` (Enum: `qcow2`, `raw`, `vmdk`) | no | `raw` | The disk format every imported image is converted to (`[image_conversion] output_format`). `raw` is the format an RBD store clones copy-on-write; `qcow2` stays sparse on a filesystem store |

`injectMetadata` carries two:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `properties` | `map[string]string` (MinProperties 1, MaxProperties 64, name and value each ≤ 255 characters) | yes | — | The properties injected onto every image the plugin applies to (`[inject_metadata_properties] inject`). They are how a deployment marks provenance or pins scheduling and driver behaviour on images it did not build itself, without trusting the importing user to set them. Rendered with the keys sorted alphabetically, so an unordered map cannot reshuffle the config and roll the Deployment on a reconcile that changed nothing |
| `ignoreUserRoles` | `[]string` (MaxItems 64, item length 1–255) | no | `admin` | The Keystone roles exempt from the injection (`ignore_user_roles`): an import by a user holding one of them leaves the image's properties untouched |

The rendered order is fixed (`image_decompression`, `image_conversion`,
`inject_image_metadata`) and is not an input. Glance runs the plugins in list
order and needs decompression ahead of conversion, since converting first would
rewrite the archive instead of the disk image inside it. With no ordering knob,
no shape of this block gets that wrong.

Defaults resolve when the config is rendered, the contract
[ImportFilteringSpec](#importfilteringspec) and [ImageCacheSpec](#imagecachespec)
already follow: the defaulting webhook writes nothing back into the CR, so an
unset `outputFormat` keeps tracking `raw` and an unset `ignoreUserRoles` keeps
tracking `admin` across upgrades. Only a nil `ignoreUserRoles` is defaulted. An
explicitly empty list is honored verbatim and drops the exemption, which is how
a deployment injects for every role, admins included.

One key is rendered unconditionally and three more while their plugin is
enabled:

| Key | Rendered | Value |
| --- | --- | --- |
| `[image_import_opts] image_import_plugins` | always | The enabled plugin names in the fixed order; `[]` while `spec.importPlugins` is nil |
| `[image_conversion] output_format` | `conversion` set | The resolved output format |
| `[inject_metadata_properties] inject` | `injectMetadata` set | `key:value` pairs joined with commas, keys sorted |
| `[inject_metadata_properties] ignore_user_roles` | `injectMetadata` set | The roles joined with commas, without brackets: the option is an unbounded oslo `ListOpt`, which would read a bracket as part of the first and last role name |

`image_import_plugins` is written on every reconcile, the empty list included,
for the reason the filter lists are. An absent key leaves Glance on its own
default; the empty list is what says no plugin. The operator upgrade that
introduces this block therefore re-renders every existing Glance once, which
rewrites the content-hashed config ConfigMap and rolls the Deployment. That roll
happens once, with no CR edit, and the rendered list is `[]` until a CR asks for
a plugin. Every later change to the block costs one further rollout.

**The plugins reach the interoperable import flow and nothing else.** They are
taskflow stages of `POST /v2/images/{image_id}/import`. An image uploaded with
`PUT /v2/images/{image_id}/file` never passes through them, so a deployment that
relies on conversion or on injected metadata has to keep that upload path out of
its policy. Glance also skips them for the `copy-image` import method, which
moves an image already in one store into another. `glance-direct` is not among
the import methods this operator enables (`enabled_import_methods` is pinned to
`[web-download,copy-image]`, see [StagingSpec](#stagingspec)), which leaves one
path: the plugins act on `web-download` imports.

**Decompression unpacks one layer.** Glance identifies the staged file by its
magic number and unpacks gzip, zip, and LHA in place; LHA needs the optional
`lhafile` package, which the operator's Glance image ships. A zip or LHA archive
must hold exactly one file. A multi-layer archive such as `tar.gz` is
unsupported, because unpacking the outer layer yields the tar, which is no more
a disk image than the archive was. Formats the plugin does not recognize, bzip2
and xz among them, pass through untouched and reach the store as the archive
they arrived as.

::: danger Size the staging area against the unpacked image, not the download
The plugin unpacks in place with no bound on the result, and it cannot know that
size before writing it. `image_size_cap` is no help either: it measures the
bytes transferred, so a ~10 MiB gzip that expands past 10 GiB is well under any
cap the deployment sets — gzip reaches roughly 1000:1 on crafted input. The
operator adds no bound of its own; the only one in the path is
[`spec.staging.sizeLimit`](#stagingspec), whose default is `10Gi`.

Breaching that bound evicts the glance-api pod rather than the offending import,
and within one pod it is a single shared budget rather than per-import
accounting, so **every other in-flight import on that replica dies with it**.
The blast radius is per-replica and shared: on the single-replica layout the
quick start uses, one `web-download` import of a compression bomb is a full
image-service outage, repeatable on a loop. The `import_filtering_opts`
allow-list does not catch it, since the filter inspects the URI and not the
payload.

Which is why the bound cannot stay an inherited default here: a CR enabling
`decompression` is rejected at admission unless it also sets
`spec.staging.sizeLimit`, or `spec.staging.unbounded` to deliberately keep no
bound. That is an acknowledgement, not a control — the ratio itself is nothing
the operator can cap.

So enable `decompression` only where the import path is already restricted to
trusted callers by `oslo.policy`, and size `spec.staging.sizeLimit` against the
largest **unpacked** image the deployment expects rather than the largest
download. Where the import path must stay open, run imports on a glance-api
replica set of their own so an eviction cannot take the serving path with it —
a per-tenant import quota is the other control, and neither is something this
block can express.
:::

**Conversion shells out to `qemu-img`.** The plugin runs `qemu-img info` on the
staged file and `qemu-img convert` when the format it finds differs from
`outputFormat`. That binary comes from the `qemu-utils` package, which the
operator's Glance image ships; a plain Glance install carries neither it nor
`lhafile`.

The conversion runs on the node-local staging area, and the source and the
converted result live there together until the source is deleted. One import can
therefore draw about twice the image size from the [`spec.staging`](#stagingspec)
budget while it converts. Size that bound against the largest image the
deployment imports, measured at its full virtual size: the compressed file that
arrives over the wire hides that number.

A failing `qemu-img` call fails the import task that ran it. The image never
reaches `active` and the failure surfaces on the task; the API keeps serving.

The injected pairs render in the oslo Dict syntax, which splits the value on
commas and each pair on its first colon. A property name therefore carries
neither character, and a value may carry a colon (a URL-shaped value works) but
no comma. Map keys are reachable from no CRD marker, so every rule on a property
name lives in the validating webhook alone; see
[Defaulting and validation](#defaulting-and-validation).

All four keys are registered as operator-owned and **Rejected**: the validating
webhook refuses an `extraConfig` override of any of them, whether or not
`spec.importPlugins` is set. The block expresses everything they express, so an
override adds no reach. What it adds is a way around the fixed
decompression-before-conversion order, the output-format enum, and the
property-name rules that keep an injected pair inside the Dict syntax, leaving an
audit that reads `spec.importPlugins` seeing one pipeline while the rendered
config runs another.

::: warning Migrate these keys out of `extraConfig` before upgrading
Setting `[image_import_opts] image_import_plugins` through `extraConfig` was the
only way to enable these plugins before `spec.importPlugins` existed, so a
Glance already running conversion or metadata injection carries it. The
rejection is unconditional and applies on **update** as much as on create: once
the new operator is running, any write to such a CR is refused with a
`Forbidden` naming a field the write did not touch, so an image bump, a replica
change, or a credential rotation is blocked until the key is removed.

Move the settings onto `spec.importPlugins` **before** rolling the operator:

| `extraConfig` key | Replacement |
| --- | --- |
| `[image_import_opts] image_import_plugins` | The sub-block per plugin — `decompression`, `conversion`, `injectMetadata` |
| `[image_conversion] output_format` | `conversion.outputFormat` |
| `[inject_metadata_properties] inject` | `injectMetadata.properties` |
| `[inject_metadata_properties] ignore_user_roles` | `injectMetadata.ignoreUserRoles` |

Through a [`ControlPlane`](../c5c3/controlplane-crd.md) the same key is rejected
on `services.glance.extraConfig` **and** on the merged
`spec.globalExtraConfig`, and the consequence is worse: the ControlPlane
projects the merged `extraConfig` onto its Glance child unconditionally, so the
child's webhook refuses the projection and nothing else the ControlPlane changes
reaches the child either. That wedge is diagnosable rather than silent —
`GlanceReady` goes `False` with reason `GlanceProjectionRejected` and the
rejection message on it. Removing the key from the ControlPlane clears it.
:::

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

### ImageCacheSpec

Glance can keep a copy of the image data it has served on the API pod's own
filesystem, so a repeat download of the same image is answered from the node
instead of from the backing store. This block is the opt-in. While
`spec.imageCache` is set the operator mounts the cache volume, renders the cache
config keys, injects the `cache` paste filter, and runs the maintenance sidecar;
with it nil the pod template and the rendered config are what they were before
the block existed.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `sizeLimit` | `*resource.Quantity` | no | `10Gi` | The `emptyDir.sizeLimit` stamped on the per-replica `image-cache` volume, mounted at `/var/lib/glance/image-cache` in the `glance-api` container. Must be at least `1Mi` |
| `maintenanceInterval` | `*metav1.Duration` | no | `5m` | How often the `cache-maintenance` sidecar runs `glance-cache-pruner` and `glance-cache-cleaner`. Must be at least `1m` |

Both fields resolve at reconcile time, the contract
[ImportFilteringSpec](#importfilteringspec), [DBPurgeSpec](#dbpurgespec) and
[StagingSpec](#stagingspec) already follow: the defaulting webhook writes nothing
back into the CR, so an empty block and a set block leaving one field unset both
keep tracking the operator default across upgrades.

**The cache is per replica, and it is not shared.** Every pod fills its own copy,
an image is cached once per replica that served it, and a request is answered
from cache only when it lands on a replica that already holds the image. Three
replicas therefore claim three times `sizeLimit` of node disk, and the same
image is pulled from the store up to three times before every replica holds it.
Hit rate rises as replicas fall, which is the one place where this block and the
availability argument for more replicas pull in opposite directions.

**Every rollout starts the cache cold.** It lives in an `emptyDir`, so any pod
replacement (a config change, a release upgrade, a node drain, an eviction)
discards it and the first read of each image goes to the backing store again.
Nothing warms it in advance, and no CR field changes that.

Three `[DEFAULT]` keys are rendered, and only while the block is set:

| Key | Value |
| --- | --- |
| `image_cache_dir` | `/var/lib/glance/image-cache` |
| `image_cache_driver` | `sqlite` |
| `image_cache_max_size` | 80% of `sizeLimit`, in bytes |

`image_cache_stall_time` (86400) and `image_cache_sqlite_db` (`cache.db`) stay
unrendered. Glance's own defaults for those two are already what this deployment
wants, and rendering them would claim ownership of two more keys for no gain.
The three above are registered as operator-owned and **Rejected**: the
validating webhook refuses an `extraConfig` override of any of them, whether or
not `spec.imageCache` is set. `extraConfig` wins over every operator default in
the merge, and `ExtraConfigHealthy` is informational — it never gates `Ready` —
so each of these overrides would be damage already done by the time a condition
could report it. `image_cache_max_size` at or above the `emptyDir` bound leaves
the pruner nothing to prune down to, so the volume grows until the kubelet
evicts the pod. `image_cache_dir` elsewhere does not create an unbounded path —
the root filesystem is read-only, so every writable path in the pod is already
a bounded `emptyDir` — it spends a different volume's bound, filling the
staging or tasks-work budget and evicting the pod mid-import.
`image_cache_driver` is covered below.

`image_cache_max_size` is derived as `Value()/10*8`, so a `10Gi` bound renders
`8589934592` and a `256Mi` bound `214748360`. The division runs before the
multiplication, which keeps the truncation below the true 80% and never above
it. The band matters because glance's pruner only prunes *down to*
`image_cache_max_size`, and only when the maintenance loop runs it, so the cache
legitimately sits above that mark between two passes. The remaining 20% is the
headroom those writes have before they cross the `emptyDir` bound and the
kubelet evicts the pod. What bounds how much of that headroom one burst of
downloads may eat is the sidecar's 30-second size poll, not
`maintenanceInterval` — see the maintenance loop below.

A single image larger than `image_cache_max_size` is still cached in full: the
size check happens after the download, and the next pruner pass removes it. Such
an image only becomes a problem once it approaches `sizeLimit` itself, the bound
the kubelet enforces and the one glance knows nothing about.

**The driver is pinned to `sqlite`.** Upstream marks that driver deprecated in
favour of `centralized_db`, and the operator chooses it anyway.
`centralized_db` keys cache state in the Glance database per
`worker_self_reference_url`, and Deployment pods get a new name on every
replacement: each roll would strand the dead pods' `node_reference` and
`cached_images` rows there, and every cache hit would carry a database write.
Nothing reclaims those rows: the `db-purge` CronJob sweeps the tasks table, the
image child tables and the images table, and the pruner and cleaner key on the
running pod's own `worker_self_reference_url`, so a surviving replica never sees
a dead one's entries. `sqlite` keeps its metadata in `cache.db` inside the cache
directory, so cache metadata shares the volume's lifecycle and the volume the
pod's, and a replaced pod leaves nothing behind.

**The `cache` paste filter is injected, and its name is reserved.** The operator
appends the filter directly before the root app, after every `spec.middleware`
entry positioned `after`:

```text
cors http_proxy_to_wsgi versionnegotiation authtoken context [middleware…] cache rootapp
```

Glance's cache filter applies a single policy rule, `download_image`, on
`GET /v2/images/{id}/file`. Because the operator writes the `[filter:cache]`
section itself, a `spec.middleware` entry named `cache` would define that filter
a second time, and the validating webhook rejects it for as long as
`spec.imageCache` is set. With the block nil the name stays free, so a CR that
already carries such a middleware is unaffected.

**The pod gains a volume and a container.** An `image-cache` `emptyDir` bounded
by the resolved `sizeLimit`, mounted read-write into `glance-api`, and a second
container named `cache-maintenance` running the same image under the restricted
security context, with fixed requests of `25m` CPU and `64Mi` memory and a
`256Mi` memory limit. Those requests are not cosmetic: the HPA emits a
pod-scoped `Resource` metric, so one container without a CPU request makes the
metric unavailable for the whole pod and silently freezes `spec.autoscaling`.
The sidecar carries no environment either: both CLIs read the mounted config
directory and touch only the cache directory and its sqlite metadata, so they
open neither the database nor `glance_store`, and they make no network calls,
which is why `spec.networkPolicy` needs no rule for them.

**The loop runs on cache size first and the clock second.** Glance applies no
write barrier of its own — `image_cache_max_size` is read by the pruner, never
by the API's cache filter — so a purely time-driven loop would let concurrent
downloads pile into the `emptyDir` between two passes and cross the bound the
kubelet does enforce. The sidecar therefore re-reads the directory every 30
seconds and prunes as soon as it sits above `image_cache_max_size`, whatever
`maintenanceInterval` says. That interval remains the cadence for the passes
that run while the cache is under the mark, which is what bounds how long a
stalled entry survives.

The size poll deliberately measures less than the directory holds: the
`incomplete/`, `invalid/` and `queue/` subdirectories are excluded, because
their entries belong to `glance-cache-cleaner` and only once
`image_cache_stall_time` (a day) has passed. Counting them would leave the size
trigger latched on for that whole day over bytes no prune could release. A poll
that fails to measure at all is logged and runs a pass anyway rather than read
an unreadable directory as an empty cache.

**No maintenance failure takes the pod down, and none of them reach the control
plane.** The loop reports each failed pass and retries on the next one, however
long the failure lasts. The sidecar shares the pod with the API container, so an
exiting loop would `CrashLoopBackOff` the pod into `NotReady` and drop that
replica from the Service — and a permanently broken pruner (a corrupted sqlite
index, a renamed CLI after an image bump) is a property of the image and the
shared config, so it fails on every replica at once. Escalating it would turn a
degraded cache into an immediate Glance outage.

Absorbing it is not free either, and the CR will not tell you so. Nothing else
enforces `image_cache_max_size`, so under a pruner that never succeeds every
replica's cache climbs to the `emptyDir` bound and the kubelet evicts it on
ephemeral-storage pressure. That eviction is a node-local decision that never
passes through the eviction API, so the `PodDisruptionBudget` the operator
creates does **not** stagger it: replicas under even traffic cross their
identical bound at roughly the same time and go together. What the `Glance` CR
shows is `DeploymentReady=False` with reason `WaitingForDeployment` and nothing
naming the cause — no condition, no event, and no metric tracks this loop.

The sidecar's log is therefore the only signal, and it is the one to alert on:

| Line | Meaning |
| --- | --- |
| `glance-cache-maintenance failed: <n> consecutive, cache at <used> KiB of <high water>` | A pass failed. `1 consecutive` on an otherwise quiet log is a transient (a lost sqlite lock); a count that keeps climbing is a pruner that can never run, and the evictions above are what comes next |
| `glance-cache-maintenance recovered: after <n> consecutive failures` | A pass succeeded again — what closes an alert opened on the line above |
| `glance-cache-maintenance unmeasured: could not measure <dir>; running a pass anyway` | The size poll failed; `du`'s own error is on the line before it |

All three go to stderr, apart from the two CLIs' own output, and are read with
`kubectl logs -c cache-maintenance`.

Enabling the cache, resizing it, or retuning the interval rolls the Deployment
once. The pod template and the config hash change in the same reconcile, so the
two do not produce separate rollouts. The effective bound is readable from the
live Deployment, and the volume is absent while the block is nil:

```bash
kubectl get deploy glance -n openstack \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="image-cache")].emptyDir.sizeLimit}{"\n"}'
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

`spec.imageCache` is in the same position, one field over. The defaulting
webhook leaves it untouched, and both of its floors are webhook-only: a
`resource.Quantity` renders as `x-kubernetes-int-or-string` and a
`metav1.Duration` as a plain string, and neither shape carries a `Minimum`
marker, so no CEL rule can express "at least `1Mi`" or "at least `1m`". The same
exported validator admits `spec.services.glance.imageCache` on a `ControlPlane`.
The webhook additionally rejects a `spec.middleware` entry named `cache` while
the block is set, since the operator injects a paste filter of that name itself
(see [ImageCacheSpec](#imagecachespec)).

`spec.importPlugins` is untouched by the defaulting webhook for the same reason:
an unset `conversion.outputFormat` resolves to `raw` and an unset
`injectMetadata.ignoreUserRoles` to `admin` when the config is rendered, and a
nil block enables no plugin. The validating webhook mirrors the schema as
defense in depth (the output-format enum, the 1–64 property count, the 64-item
and 1–255-character bounds on `ignoreUserRoles`, and the 255-character bound on
each half of an injected property) and adds the rules the schema cannot reach. A
map key carries no marker of its own, so every check on an injected property
name apart from its length is webhook-only: it must be non-empty, must not start
or end with whitespace, and must carry no colon, comma, newline, or carriage
return, because the rendered `[inject_metadata_properties] inject` value is
parsed as an oslo Dict that splits pairs on commas and each pair on its first
colon. A property value is checked for the same characters minus the colon,
which belongs to it. Length is the one rule with a schema counterpart for both
halves — a CEL rule on `properties` — because every pair renders verbatim into
the one `inject` line, and an unbounded value would let a CR that etcd accepts
render a `glance-api.conf` past the 1 MiB ConfigMap ceiling, which the API
server then refuses on every reconcile. Each `ignoreUserRoles` item is checked
for commas and control characters, since that list renders as a plain comma
join. A `ControlPlane` calls the same exported validator on
`spec.services.glance.importPlugins`, so both CRs admit the same values.

One more `importPlugins` rule correlates two top-level fields, which puts it out
of reach of any marker on either type: a CR enabling `decompression` must also
set `spec.staging.sizeLimit`, or `spec.staging.unbounded` to deliberately keep
no bound at all. The plugin expands the staged image by a ratio the caller
picks, which makes that bound the only one in the path, and the operator default
was sized against the largest download rather than the largest unpacked image —
so admission asks for the one thing it can, that somebody chose the number. A
`ControlPlane` enforces the same pairing on `spec.services.glance`, since it
projects both blocks onto its Glance child untouched.

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
`spec.serviceUser.secretRef`), the six `[import_filtering_opts]` keys (owned
via `spec.importFiltering`), the three `[DEFAULT] image_cache_*` keys (owned
via `spec.imageCache`), and the four image-import plugin keys
(`[image_import_opts] image_import_plugins`, `[image_conversion] output_format`,
`[inject_metadata_properties] inject` and `ignore_user_roles`, owned via
`spec.importPlugins`).

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
