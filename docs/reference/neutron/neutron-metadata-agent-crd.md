---
title: NeutronMetadataAgent CRD
quadrant: operator
---

# NeutronMetadataAgent CRD

`neutronmetadataagents.neutron.openstack.c5c3.io/v1alpha1`, kind
`NeutronMetadataAgent`. The CRD is generated from
`operators/neutron/api/v1alpha1/neutronmetadataagent_types.go`; the defaulting
and validating webhooks live in
`operators/neutron/api/v1alpha1/neutronmetadataagent_webhook.go`.

One CR describes one `neutron-ovn-metadata-agent` DaemonSet. It runs on the
nodes an [`OVNChassis`](../ovn/ovn-chassis-crd.md) selects, reads the local Open
vSwitch database the chassis pods create, and answers the 169.254.169.254
requests the instances on that node make. What it serves comes out of the OVN
Southbound database: the agent watches the port bindings of its own node and
builds one metadata proxy per network from them.

The kind is separate from [`Neutron`](./neutron-crd.md) because it belongs in
the chassis's namespace. `spec.chassisRef` is namespace-local, the pods mount the
chassis's runtime directory and the client Secret the chassis's `OVNCentral`
publishes, and neither crosses a namespace. That namespace runs a privileged,
host-network workload, which the Neutron API Deployment's namespace does not;
see [Target Clusters](../target-clusters.md).

`kubectl get neutronmetadataagents` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Desired
(`.status.desiredNumberScheduled`), Ready pods (`.status.numberReady`), and Age.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `openStackRelease` | `string` (Pattern `^\d{4}\.[12]$`) | yes | none | The OpenStack release this agent runs. It selects the option catalog `spec.extraConfig` is validated against and nothing else: the agent installs no schema and tracks no installed release. The `[12]` minor class keeps the CRD pattern, the webhook and `release.ParseRelease` in agreement, so a non-cadence minor is rejected at every layer |
| `image` | [`commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | yes | none | The Neutron container image the agent runs from. Required with no operator-resolved fallback: the agent is deployed next to an `OVNChassis` whose image this operator does not resolve, so there is no tested pairing to fall back on |
| `chassisRef` | [`OVNChassisRef`](#ovnchassisref) | yes | none | The `OVNChassis` this agent runs alongside. It supplies the node selector, the tolerations and, through that chassis's `OVNCentral`, the Southbound address and the client Secret. Immutable, enforced by a CEL transition rule and by the webhook |
| `messaging` | [`commonv1.MessagingSpec`](../c5c3/controlplane-crd.md#messagingspec) pointer | no | `nil` | The RabbitMQ connection. Optional, because the agent opens no RPC and no notification connection of its own. It exists so a deployment can give the agent the same bus configuration the API pods carry: `config.init` calls `n_rpc.init` unconditionally, which parses oslo.messaging's default `rabbit://` URL without dialing it. When set, the agent gets the same `OS_DEFAULT__TRANSPORT_URL` override the API pods get, and the `[oslo_messaging_rabbit]` section is rendered |
| `novaMetadata` | [`NovaMetadataSpec`](#novametadataspec) pointer | no | `nil` | The Nova metadata API the agent proxies to. Nova is not onboarded onto this operator, so a nil block renders neither key and the oslo defaults apply |
| `resources` | `corev1.ResourceRequirements` | no | `{}` | Requests and limits for the init container and the agent container, applied to both. An empty block falls back at reconcile time to the shared container defaults a defaulted `DeploymentSpec` carries (100m/500m CPU, 256Mi/512Mi memory), so a CR that names none still lands in the Burstable QoS class instead of BestEffort |
| `logging` | [`*LoggingSpec`](../keystone/keystone-crd.md#loggingspec) | no | `text` / `INFO` / `debug: false` | oslo.log derivation: `format` (`text` or `json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook. The `json` format ships a `logging.conf` in the config ConfigMap and points `[DEFAULT] log_config_append` at it |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the DaemonSet, the config ConfigMaps and the transport-URL Secret are created on. The CR itself, its status and its finalizer stay on the management cluster. Immutable, enforced by two CEL transition rules and by the webhook. It has to name the same cluster the referenced `OVNChassis` names |
| `extraConfig` | `map[string]map[string]string` | no | `nil` | Free-form INI sections for `neutron_ovn_metadata_agent.ini` options with no dedicated field. The render-time merge is `operator defaults < extraConfig`, so a user value wins. An override of an operator-owned key is honored and reported through the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event, except for the six keys the webhook refuses outright. Option names are checked at admission against the per-release catalog embedded in the operator |

`LoggingSpec` is a Go alias of the shared `commonv1` definition, which carries
the per-field godoc and the validation markers.

### OVNChassisRef

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | none | The `OVNChassis`'s name. There is no namespace field: the reference is resolved in this CR's own namespace, because the agent mounts the chassis's runtime directory and the Secret holding its Southbound client certificate |

### NovaMetadataSpec

The agent terminates the instance's request to 169.254.169.254 and forwards it
to the Nova metadata API with the instance identity attached, signed with the
shared secret both sides carry.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `host` | `string` | no | `""` (the oslo default) | The address of the Nova metadata API, rendered as `[DEFAULT] nova_metadata_host`. An empty value omits the key |
| `port` | `int32` (Minimum=1, Maximum=65535) | no | `8775`, filled by the defaulting webhook | The port the Nova metadata API listens on, rendered as `[DEFAULT] nova_metadata_port`. 8775 is `nova-api-metadata`'s own default |
| `sharedSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | no | `nil`; `key` webhook-defaulted to `shared_secret` | The Secret holding the value the agent signs forwarded requests with. Nova rejects an unsigned request when it carries a secret of its own, so the two values have to match. The value reaches the process as `OS_DEFAULT__METADATA_PROXY_SHARED_SECRET` and never enters the rendered ConfigMap |

## Defaulting and validation

The mutating webhook does three things. It materializes `spec.logging` and its
baseline (`text` / `INFO` / `debug: false`) so no reconciler dereferences a nil
pointer. Inside a present `spec.novaMetadata` it fills a zero `port` with 8775
and an empty `sharedSecretRef.key` with `shared_secret`. A nil
`spec.novaMetadata` stays nil, because the agent then renders neither key.

Two defaults are resolved at reconcile time and never written into the stored
CR: the container resources fall back to the shared `DeploymentSpec` values, and
a `spec.messaging.secretRef` without a `key` reads `transport_url`.

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `NeutronMetadataAgentSpec`, one for adding or removing the ref and one for renaming it |
| `chassisRef is immutable` | A third transition rule on the same spec. An agent re-pointed at another chassis lands on another set of nodes, whose local Open vSwitch databases carry none of the ports it was answering for |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec` on `spec.image` |
| `exactly one of clusterRef or secretRef must be set` | Inherited from `commonv1.MessagingSpec` on `spec.messaging` |

The three transition rules are evaluated on UPDATE only. Beside them the schema
carries the ordinary field markers: the `^\d{4}\.[12]$` pattern on
`spec.openStackRelease`, `MinLength=1` on `spec.chassisRef.name`, and
`Minimum=1` / `Maximum=65535` on `spec.novaMetadata.port`.

### Webhook rules

The validating webhook accumulates every violation into one admission response.
Most rules repeat a schema-layer check so the invariant survives an object that
reached etcd through a bypass; the `extraConfig` rules have no schema
counterpart at all.

The name bound, the chassis and the Nova metadata block:

| Message | Trigger |
| --- | --- |
| `name must be at most %d characters: it is the app.kubernetes.io/instance label value on every child and Kubernetes caps label values at %d characters` | `metadata.name` longer than `MaxNeutronMetadataAgentNameLength`. Both arguments are that bound, 63. Enforced on create alone |
| `chassisRef.name must be set (the OVNChassis this agent runs alongside)` | `spec.chassisRef.name` is empty. The chassis is what puts the agent on a node and gives it the local Open vSwitch database to read |
| `chassisRef is immutable` | An update renames `spec.chassisRef.name`, the webhook-layer twin of the CEL rule |
| `port must be between 1 and 65535` | `spec.novaMetadata.port` outside the range. The defaulting webhook fills a zero with 8775, so this fires only for an object that bypassed it |
| `sharedSecretRef.name must be set when spec.novaMetadata.sharedSecretRef is configured` | The ref is present with an empty `name` |

Image, messaging and the target cluster, through the shared helpers:

| Message | Trigger |
| --- | --- |
| `repository must be set` | `spec.image.repository` is empty |
| `exactly one of image.tag or image.digest must be set` | Both or neither are set on `spec.image`, re-checked outside the schema |
| `exactly one of clusterRef or secretRef must be set` | A present `spec.messaging` names both modes or neither. The block is optional here, so the check runs only once it is set |
| `target cluster name must be set` | `spec.targetClusterRef` is present with an empty `name` |
| `targetClusterRef is immutable (adding or removing it after creation is not permitted)` | An update adds or drops the ref. Both strand the children already created on the previously selected cluster |
| `targetClusterRef is immutable (the children already exist on the previously named cluster)` | An update renames the ref |

Control characters. Both values below are written into the rendered INI
verbatim, so a newline would inject a whole config line past the
`(section, key)`-keyed ownership and catalog gates:

| Message | Applies to |
| --- | --- |
| `value must not contain a newline or carriage return: it is rendered verbatim into neutron_ovn_metadata_agent.ini, so a newline injects arbitrary config lines` | `spec.chassisRef.name`, which is resolved into the `[ovs]` and `[ovn]` connection strings, and `spec.novaMetadata.host`, which is rendered as a `[DEFAULT]` option |

Logging, through the shared logging validator:

| Message | Trigger |
| --- | --- |
| `field.NotSupported` on `spec.logging.format` | A format outside `text` and `json` |
| `field.NotSupported` on `spec.logging.level` | A level outside `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL` |
| `logger name must not be empty` | A `perLoggerLevels` entry keyed on the empty string |
| `logger name must not contain a newline or carriage return: it is rendered verbatim into neutron_ovn_metadata_agent.ini, so a newline injects arbitrary config lines` | A logger name with a control character. The name renders into the `[DEFAULT] default_log_levels` CSV |
| `level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL` | A `perLoggerLevels` value outside the set |

`spec.extraConfig` is a preserve-unknown-fields map CEL cannot constrain, so
every guard on it is the webhook's alone:

| Message | Trigger |
| --- | --- |
| `extraConfig section name must not be empty` | A section keyed on the empty string, which would render a nameless `[]` header |
| `extraConfig section name must not contain a newline or carriage return` | A control character in a section name |
| `extraConfig key must not be empty` | An option keyed on the empty string, which would render a bare `= value` line |
| `extraConfig key and value must not contain a newline or carriage return: the rendered INI writes them verbatim, so a newline injects arbitrary config lines` | A control character in an option key or value |
| `%s is managed via %s and must not be set in extraConfig (%s)` | An override of a `Rejected` owned key. The arguments are the key, the field or computation that owns it, and its impact; the parenthesis is dropped for an entry with no impact text |
| `no such option in the neutron %s option catalog` | An option name the release catalog does not know. The argument is the catalog's release |
| `no such section in the neutron %s option catalog` | A section name the release catalog does not know |

Both kinds share one catalog, the flat union of `neutron.conf`, `ml2_conf.ini`
and `neutron_ovn_metadata_agent.ini` with no per-file provenance. A section that
belongs to the API process therefore passes here, and oslo.config ignores it at
runtime. Owned keys are exempt from the scan, so an operator-owned key never
doubles as an unknown option. The check re-runs on update only when
`spec.extraConfig` or `spec.openStackRelease` changed.

The same check produces four admission warnings:

```text
spec.extraConfig was not validated against an option catalog: spec.openStackRelease does not name an OpenStack release
spec.extraConfig was not validated against an option catalog: no catalog for release %q is embedded in this operator build
spec.extraConfig [%s] %s: deprecated option in neutron %s, replaced by %s
spec.extraConfig [%s] %s: deprecated option in neutron %s with no replacement
```

The first two are the fail-open paths: an unresolvable release skips the option
check and raises one warning.

### Rejected owned keys

`config_ownership.go` keeps a registry per kind, and
`MetadataAgentOwnedConfigKeys` is the agent's. It is separate from the `Neutron`
registry because the two kinds render different files: the agent's `[ovn]`
section carries only the Southbound half, and its `[DEFAULT]` carries the Nova
metadata keys the API never writes. Six of its entries are refused in
`spec.extraConfig` outright:

| Key | Owned by | Why the override is refused |
| --- | --- | --- |
| `[DEFAULT] metadata_proxy_shared_secret` | `spec.novaMetadata.sharedSecretRef` | The shared secret is env-injected from the referenced Secret, so a file override is ignored at runtime and copies credential material into the rendered config Secret |
| `[ovs] ovsdb_connection` | `spec.chassisRef` | The agent reads the local Open vSwitch database over the socket the chassis pods share; another address points it at a node whose ports it is not answering for |
| `[ovn] ovn_sb_connection` | `spec.chassisRef` | The connection string is resolved from the `OVNCentral` the referenced chassis registers with; another address points the agent at a logical model it does not serve |
| `[ovn] ovn_sb_private_key` | operator-computed | The operator mounts the client keypair the `OVNCentral` issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity |
| `[ovn] ovn_sb_certificate` | operator-computed | The certificate half of the same keypair |
| `[ovn] ovn_sb_ca_cert` | operator-computed | The CA bundle verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision |

Every other entry in the registry is honored and reported: `[DEFAULT]`
`state_path`, `debug`, `nova_metadata_host` and `nova_metadata_port`, the
`[oslo_messaging_notifications] driver`, the five `[oslo_messaging_rabbit]`
keys, and `[oslo_concurrency] lock_path`. The broker keys are registered
unconditionally although they render only while `spec.messaging` is set: the
registry records that a key is not the user's to set, not that it is currently
rendered.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `installedImage` | `string` | The image reference the running DaemonSet was projected from, recorded once the DaemonSet reports a ready pod on every node it schedules. A rollout that has reached no node leaves the previous value in place, which is what tells the two apart |
| `desiredNumberScheduled` | `int32` | How many nodes the DaemonSet should run on, mirrored from the DaemonSet status |
| `numberReady` | `int32` | How many of those nodes have a ready agent pod |

### Conditions

Three sub-reconcilers each own one condition type, and every one of the three is
set on every pass: the agent runs no optional step. The aggregate `Ready` is
`True` only when all three are. For the pipeline that sets them see
[Reconciler Architecture](./neutron-reconciler.md).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `ChassisReady` | True | `ChassisResolved` | The `OVNChassis` resolved and its `OVNCentral` published both a Southbound address and a client Secret name. The message names the chassis, the central and the address |
| `ChassisReady` | False | `ChassisNotFound` | No `OVNChassis` of that name in this CR's namespace. An agent applied before its chassis is an ordinary ordering of two objects in one manifest, so this polls |
| `ChassisReady` | False | `ChassisReadError` | Reading the `OVNChassis` failed |
| `ChassisReady` | False | `ChassisOnAnotherCluster` | The two CRs name different target clusters. The agent shares the chassis's nodes and mounts the Secret the central publishes, and neither a node nor a Secret crosses a cluster boundary. No requeue: both refs are immutable, so only deleting and reapplying one of the two can repair it |
| `ChassisReady` | False | `CentralNotFound` | The `OVNCentral` the chassis attaches to does not exist in this namespace |
| `ChassisReady` | False | `CentralReadError` | Reading the `OVNCentral` failed |
| `ChassisReady` | False | `CentralNotReady` | The central has published no Southbound address or no client Secret name yet |
| `ChassisReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve. This is the pipeline's first gate, so the failure lands on the condition the rest of the graph waits behind. A deleting CR waiting for its target cluster to come back reports the same reason here |
| `SecretsReady` | True | `SecretsAvailable` | Every credential the agent needs is there. A CR that sets neither `spec.novaMetadata.sharedSecretRef` nor `spec.messaging` reaches this without reading anything |
| `SecretsReady` | False | `WaitingForNovaSharedSecret` | The Secret named by `spec.novaMetadata.sharedSecretRef` is missing, or carries no value under the configured key. The message distinguishes a missing ExternalSecret from one that has not synced and from a Secret missing keys |
| `SecretsReady` | False | `WaitingForMessagingCredentials` | The managed `RabbitmqCluster` has published no default-user Secret yet, or the brownfield Secret carries no transport URL |
| `SecretsReady` | False | `ConfigError` | Rendering or pruning the config ConfigMap failed. Config artefacts gate the same downstream step as the credentials, so they share this condition and there is no separate one for them |
| `DaemonSetReady` | True | `DaemonSetReady` | The DaemonSet runs a ready pod on every node it schedules. The message counts the nodes |
| `DaemonSetReady` | False | `DaemonSetProgressing` | Fewer nodes run a ready pod than the DaemonSet schedules. The message counts both |
| `DaemonSetReady` | False | `DaemonSetError` | The DaemonSet could not be applied or read |
| `Ready` | True | `AllReady` | All three sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |

`ExtraConfigHealthy` is set beside these on every pass that renders a config,
reporting the honored overrides of operator-owned keys. It is informational and
stays out of the `Ready` aggregation.

## Sub-Resource Naming Convention

Every child takes the CR name plus a component suffix. For a
`NeutronMetadataAgent` named `{name}`:

| Resource | Name | Notes |
| --- | --- | --- |
| DaemonSet | `{name}-metadata-agent` | The `neutron-ovn-metadata-agent` container plus the `wait-for-chassis` init container, on the nodes the referenced `OVNChassis` selects. The suffix is also the `app.kubernetes.io/component` label value and the container name |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `neutron_ovn_metadata_agent.ini`, plus `logging.conf` for `spec.logging.format: json`; 3 historical retained |
| Secret | `{name}-transport-url` | The derived `rabbit://` URL, written only while `spec.messaging` is set and consumed through `OS_DEFAULT__TRANSPORT_URL` |

The `app.kubernetes.io/name` label on every child is `neutronmetadataagent`, the
kind in lower case, which is what keeps these children apart from the ones a
`Neutron` of the same name projects into the same namespace. The pod selector
narrows that label set by the component, so the DaemonSet selects its own pods
and not the chassis pods sharing the node.

`metadata.name` is bounded at 63 characters, because it is the
`app.kubernetes.io/instance` label value on every child and the owner-name label
value on the children of a placed CR. Kubernetes caps a label value at 63.

## Node contract

The agent picks no nodes of its own. `spec.nodeSelector` and `spec.tolerations`
of the referenced `OVNChassis` are copied verbatim onto the DaemonSet, so the
agent lands on the set of nodes the chassis programs and nowhere else. Both are
deep-copied, and `spec.chassisRef` is resolved in the CR's own namespace through
the management-cluster client, since every CR of this control plane is written
there whatever cluster the children land on.

The pod runs with `hostNetwork: true`: it answers the 169.254.169.254 requests
arriving on the node's own interfaces. Two host paths are mounted, both
`DirectoryOrCreate` so a node that has never run Open vSwitch still starts:

| Path | Mount | Why it is a host path |
| --- | --- | --- |
| `/run/openvswitch` | read-write, on the init container and the agent container | The local Open vSwitch database socket the chassis pods create. It is what the agent reads its node's port bindings from, and an emptyDir would leave it reading nothing |
| `/run/netns` | read-write with `Bidirectional` mount propagation | The network namespaces the agent creates. Bidirectional propagation is what makes them visible to the node, which is how the datapath reaches the proxies running inside them |

The agent container runs privileged and pinned to uid 0, with
`runAsNonRoot: false`. It creates network namespaces, moves interfaces into them
and starts a haproxy per network through privsep, which the Restricted profile
denies and no named capability covers; privsep-helper is invoked through `sudo`,
so the image's own unprivileged user does not work either. The pod-level security
context carries the seccomp profile and nothing else: no `fsGroup`, because it
would be applied to the host directories the pod mounts, where the ownership is
the node's business. The `wait-for-chassis` init container runs under the
Restricted profile.

That init container is the same-node gate. It polls the local database until the
chassis has registered itself:

```text
until ovsdb-client --timeout=5 transact unix:/run/openvswitch/db.sock \
  '["Open_vSwitch",{"op":"select","table":"Open_vSwitch","where":[],"columns":["external_ids"]}]' \
  2>/dev/null | grep -q system-id; do sleep 2; done
```

`external_ids:system-id` is what the chassis's own `apply-node` init container
writes, and until that row exists the agent has no chassis to read port bindings
for. Both workloads select the same nodes and nothing orders the two DaemonSets,
so the gate is per node rather than per cluster. The query goes through
`ovsdb-client` because the neutron image ships the OVS Python client without the
`ovs-vsctl` binary.

Readiness is the metadata proxy socket, tested with
`test -S /var/lib/neutron/metadata_proxy` after a 5 s initial delay, every 5 s,
with a 5 s timeout. An agent that has not opened that socket answers no instance
and must not count as a node the rollout may move on from.

There is no liveness probe. An agent that lost its Southbound connection keeps
serving the proxies it already created, and a restart would drop them for a
fault the readiness probe already reports.

## Example

```yaml
apiVersion: neutron.openstack.c5c3.io/v1alpha1
kind: NeutronMetadataAgent
metadata:
  name: neutron-agent
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  image:
    repository: ghcr.io/c5c3/neutron
    tag: "2025.2"
  chassisRef:
    name: neutron-agent-chassis
```

This is the fixture the `metadata-agent` suite applies
(`tests/e2e/neutron/metadata-agent/03-neutronmetadataagent-cr.yaml`). It leaves
`spec.messaging` out, which is what gives the suite's "no `transport_url` line"
assertion something to check, and `spec.novaMetadata` too, since Nova is not
onboarded onto this operator. Without either block the CR references no Secret
at all, the path `SecretsReady=True` with reason `SecretsAvailable` covers.
