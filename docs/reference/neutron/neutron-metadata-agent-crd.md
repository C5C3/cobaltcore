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
| `openStackRelease` | `string` (Pattern `^\d{4}\.[12]$`) | yes | none | The OpenStack release this agent runs. It selects the option catalog `spec.extraConfig` is validated against and nothing else: the agent installs no schema and tracks no installed release. The `[12]` minor class keeps the CRD pattern, the webhook and `release.ParseRelease` in agreement, so a non-cadence minor is rejected at every layer. The validating webhook rejects a value below `2026.1`, the oldest supported release, on create and on change, and warns on an unchanged one. |
| `image` | [`commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | yes | none | The Neutron container image the agent runs from. Required with no operator-resolved fallback: the agent is deployed next to an `OVNChassis` whose image this operator does not resolve, so there is no tested pairing to fall back on |
| `chassisRef` | [`OVNChassisRef`](#ovnchassisref) | yes | none | The `OVNChassis` this agent runs alongside. It supplies the node selector, the tolerations, the client Secret its pods mount (`status.clientSecretName`) and, through that chassis's `OVNCentral`, the Southbound address. Immutable, enforced by a CEL transition rule and by the webhook |
| `messaging` | [`commonv1.MessagingSpec`](../c5c3/controlplane-crd.md#messagingspec) pointer | no | `nil` | The RabbitMQ connection. Optional, because the agent opens no RPC and no notification connection of its own. It exists so a deployment can give the agent the same bus configuration the API pods carry: `config.init` calls `n_rpc.init` unconditionally, which parses oslo.messaging's default `rabbit://` URL without dialing it. When set, the agent gets the same `OS_DEFAULT__TRANSPORT_URL` override the API pods get, and the `[oslo_messaging_rabbit]` section is rendered |
| `novaMetadata` | [`NovaMetadataSpec`](#novametadataspec) pointer | no | `nil` | The Nova metadata API the agent proxies to. A nil block renders none of its four keys and the oslo defaults apply, which is what an agent standing beside a control plane that runs no compute service wants |
| `metadataWorkers` | `*int32` (Minimum=0) | no | operator-resolved `4` | Rendered as `[DEFAULT] metadata_workers`. On every supported release it sizes the thread pool the agent serves metadata requests from, and `0` serves them one at a time in the main process, which is upstream's ML2/OVN default. The count does not follow the node's CPU count. The default is resolved when the config is rendered and never written into the CR |
| `resources` | `corev1.ResourceRequirements` | no | `{}` | Requests and limits for the init container and the agent container, applied to both. The operator never writes defaults into this field; it resolves them per resource when it renders the pod: a CPU the block names neither as request nor as limit gets a 230m request and no limit, and a memory it names neither way gets 2Gi as both request and limit, which fits 32 networks on the node (see [CPU sizing](#cpu-sizing), [Memory sizing](#memory-sizing) and the [resource defaults](../keystone/keystone-crd.md#resource-defaults)). A resource the block names is used as written, and anything else it sets is kept. A CR that names none still lands in the Burstable QoS class instead of BestEffort. Before the per-resource rule, the operator rendered a block that named anything as written, so a block that names only CPU gains a memory request and limit on the upgrade |
| `verticalAutoscaling` | [`*VerticalAutoscalingSpec`](../keystone/keystone-crd.md#verticalautoscalingspec) | no | `nil` | Opts the agent DaemonSet (`{name}-metadata-agent`) into a VerticalPodAutoscaler that controls the requests of its containers; see [VerticalAutoscalingSpec](../keystone/keystone-crd.md#verticalautoscalingspec). On a cluster without the VPA, `VPAReady` turns False with reason `VPANotInstalled`. |
| `logging` | [`*LoggingSpec`](../keystone/keystone-crd.md#loggingspec) | no | `text` / `INFO` / `debug: false` | oslo.log derivation: `format` (`text` or `json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook. The `json` format ships a `logging.conf` in the config ConfigMap and points `[DEFAULT] log_config_append` at it |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the DaemonSet, the config ConfigMaps and the transport-URL Secret are created on. The CR itself, its status and its finalizer stay on the management cluster. Immutable, enforced by two CEL transition rules and by the webhook. It has to name the same cluster the referenced `OVNChassis` names. The chassis's `OVNCentral` may project onto another cluster, which then has to publish its Southbound database with `externallyReachable` |
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
| `protocol` | `string` (Enum=`http`;`https`) | no | `http`, filled by the defaulting webhook | The scheme the agent forwards the instance's request with. `https` is rendered as `[DEFAULT] nova_metadata_protocol`; `http` is `nova_metadata_protocol`'s own default and omits the key, so the webhook filling it into an agent stored before the field existed does not change that agent's config and roll its DaemonSet. `http` is what an agent reaching a co-located metadata API uses. A compute cluster reached through the gateway is addressed over TLS, so an agent proxying to one sets `https` |
| `sharedSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | no | `nil`; `key` webhook-defaulted to `shared_secret` | The Secret holding the value the agent signs forwarded requests with. Nova rejects an unsigned request when it carries a secret of its own, so the two values have to match. The value reaches the process as `OS_DEFAULT__METADATA_PROXY_SHARED_SECRET` and never enters the rendered ConfigMap; its digest rides the pod template as `neutron.c5c3.io/metadata-secret-hash`, so a changed value rolls the pods. The Secret a ControlPlane generates for its compute service (`{controlplane.Name}-nova-metadata-secret`) carries the value under `shared_secret`, so naming that Secret alone is enough. An agent on a compute cluster names the copy the ControlPlane delivers there instead, `{controlplane.Name}-nova-metadata-agent-secret` (see [On a compute cluster](#on-a-compute-cluster)) |
| `caBundleSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | no | `nil`; `key` webhook-defaulted to `ca.crt` | A Secret in the agent's namespace, on the cluster its pods run on, holding the PEM bundle that signs the Nova metadata API's certificate: for an agent on a compute cluster, the issuer of the metadata Gateway listener's certificate. The operator mounts it into the agent container and renders its path as `[DEFAULT] auth_ca_cert = /etc/nova-metadata-ca/ca.crt`. Requires `protocol: https`. Without it, an `https` agent verifies the certificate against the image's default CA bundle |

#### The path of a request {#metadata-path}

The figure follows a request from an instance to the Nova metadata API. Its
numbers are the hops below.

![The path of a metadata request in six numbered hops. Hop 1: an instance calls http://169.254.169.254, an address OVN answers on the chassis of its node. Hop 2: Open vSwitch hands the request to a haproxy in the network namespace of the instance's network; the metadata agent creates one such namespace per network under /run/netns and starts one haproxy in each. Hop 3: haproxy passes the request to the metadata agent over the socket metadata_proxy. Hop 4: the agent finds the port that is asking in the Southbound database. Hop 5: the agent forwards the request to the Nova metadata API {nova}-metadata on port 8775 and signs it with the shared secret, as the header X-Instance-ID-Signature. Hop 6: the metadata API checks the signature with the same secret and resolves the instance from the mappings in the API database. One Secret, {cp}-nova-metadata-secret, carries the shared secret to both ends, as an environment variable on each. For an agent on a compute cluster two things change: it reads a copy of the secret that the c5c3-operator writes there, and it reaches the metadata API over https through the metadata Gateway.](../../diagrams/compute-metadata-path.svg)

1. The instance calls `http://169.254.169.254`. Neutron runs with
   `[ovn] ovn_metadata_enabled = true`, so OVN answers that address on the
   chassis of the instance's node.
2. Open vSwitch hands the request to a haproxy in a network namespace of its
   own. The agent creates one namespace per network under `/run/netns` and
   starts one haproxy in each, which is why its container is privileged and
   mounts `/run/netns` with bidirectional propagation.
3. haproxy passes the request to the agent over the socket
   `/var/lib/neutron/metadata_proxy`. The readiness probe of the agent tests
   for that socket.
4. The agent finds the port that is asking. It reads the local Open vSwitch
   database (`[ovs] ovsdb_connection`) and the Southbound database
   (`[ovn] ovn_sb_connection`).
5. The agent forwards the request to `nova_metadata_host` on
   `nova_metadata_port` and signs it with the shared secret. Nova checks the
   header `X-Instance-ID-Signature` against an HMAC keyed with the same value.
6. The metadata API runs with `[neutron] service_metadata_proxy = true`. It
   verifies the signature and resolves the instance from the mappings in the
   API database, which is why one metadata front end serves every cell.

The shared secret reaches both ends as an environment variable and never
enters a ConfigMap: `OS_DEFAULT__METADATA_PROXY_SHARED_SECRET` on the agent,
`OS_NEUTRON__METADATA_PROXY_SHARED_SECRET` on the metadata pods. Under a
ControlPlane both read `{controlplane.Name}-nova-metadata-secret`, which an
External Secrets `Password` generator fills once. The dashed parts of the
figure are the two differences of an agent on a compute cluster, which the
next section covers.

### On a compute cluster

An agent whose pods run on a compute cluster reaches the Nova metadata API of
its ControlPlane through the metadata Gateway
(`services.nova.metadataGateway`). Its own spec wires it:

```yaml
spec:
  targetClusterRef:
    name: compute-a
  novaMetadata:
    host: nova-metadata.example.com   # services.nova.metadataGateway.hostname
    port: 443                         # the Gateway listener's port
    protocol: https
    caBundleSecretRef:
      name: nova-metadata-ca          # signs the listener's certificate
    sharedSecretRef:
      name: controlplane-nova-metadata-agent-secret   # {controlplane.Name}-nova-metadata-agent-secret
```

The agent lives in the ControlPlane's OVN central namespace: its `chassisRef`
is namespace-local, and so is that chassis's `centralRef`. For every cluster an
agent there names `{controlplane.Name}-nova-metadata-agent-secret` on, the
ControlPlane writes that Secret into the same namespace on that cluster (see
[The generated metadata shared secret](../c5c3/controlplane-crd.md#the-generated-metadata-shared-secret)).
It carries a single key, `shared_secret`, the value the compute contract carries
as `metadata_proxy_shared_secret`, so neither the bus URL nor the service
password of the contract reaches the privileged namespace. Beside the
ControlPlane's ownership labels it carries
`neutron.openstack.c5c3.io/metadata-shared-secret-mirror: "true"`
(`MetadataSharedSecretMirrorLabel`). A local agent gets no copy; in the Nova
namespace it names the generated Secret directly.

The ControlPlane never deletes a copy. The teardown of a placed agent does,
before it releases its finalizer: it lists the labelled Secrets in its
namespace on its cluster and deletes each one that no other live agent in that
namespace on the same cluster names in `sharedSecretRef`, with a Normal
`MetadataSharedSecretMirrorReaped` event per deletion. A Secret without the
label is left alone. An agent re-pointed at another Secret leaves its copy
behind until the next agent teardown in that namespace on that cluster.

The CA bundle is not delivered. The ControlPlane names only the Gateway's
`parentRef` and hostname and does not manage its certificate, so the deployment
places the bundle in the agent's namespace, for example with trust-manager.

### Memory sizing

The `metadata-agent` container holds more than the agent process. The agent
starts its privsep daemons beside itself, and for every network with a port
bound on its node it starts one haproxy in the same container. Its working set
grows with the networks on the node, so the figure a single-process service
gets does not fit it.

A CR that names no memory gets `2Gi` as request and limit, on the agent
container and on the `wait-for-chassis` init container. The figure covers 32
networks on the node.
[Sizing Calibration](../testing/sizing-calibration.md#metadata-agent-memory)
describes the measurement and the rule behind it. A node that binds more
networks needs a larger memory value in `spec.resources`, set as request and
as limit.

An agent at its limit shows `OOMKilled` as the last state of the
`metadata-agent` container. A restart does not help. The new process provisions
every network of the node again and is killed at the same point, so the
instances on that node get no metadata until the limit is raised.

Upgrading the operator changes the pod template of every agent whose CR names
no memory. The DaemonSet replaces its pods one node at a time, and each new pod
needs `2Gi` of memory that no other pod on its node has requested. A LimitRange
whose `max` memory lies below `2Gi` rejects the pods, and the DaemonSet reports
`FailedCreate` events.

### CPU sizing

A CR that names no CPU gets a request of `230m` and no CPU limit, on the agent
container and on the `wait-for-chassis` init container. The agent idles at 1 to
2 millicores, also with 32 networks on its node. It works when a server boots
there: it provisions the server's network and answers the metadata requests of
the boot. On the metal-stack lab one boot on a new network took 11 seconds of
CPU time, and the minute in which 31 servers booted on one node averaged
`198m`. The request covers that minute with 15 % headroom.
[Sizing Calibration](../testing/sizing-calibration.md#metadata-agent-cpu)
describes the session and the rule behind the figure.

The request is the share the agent gets while every CPU of its node is busy.
Without a CPU limit a burst above it is not throttled; on a busy node it takes
longer. The minute in which 32 servers of one node were hard-rebooted together
averaged `357m`. A node on which that many servers start at once, as
after a reboot of the host, can name a larger CPU request in `spec.resources`.

Upgrading the operator changes the pod template of every agent whose CR names
no CPU, so the DaemonSet replaces its pods one node at a time. A CR that names
a CPU request or limit keeps it.

## Defaulting and validation

The mutating webhook does three things. It materializes `spec.logging` and its
baseline (`text` / `INFO` / `debug: false`) so no reconciler dereferences a nil
pointer. Inside a present `spec.novaMetadata` it fills a zero `port` with 8775,
an empty `protocol` with `http`, an empty `sharedSecretRef.key` with
`shared_secret`, and an empty `caBundleSecretRef.key` with `ca.crt`. A nil
`spec.novaMetadata` stays nil, because the agent then renders none of those
keys.

Three defaults are resolved at reconcile time and never written into the stored
CR: of the container resources, the CPU request and the memory fall back to the
agent's own figures (see [CPU sizing](#cpu-sizing) and
[Memory sizing](#memory-sizing)), a
`spec.messaging.secretRef` without a `key` reads `transport_url`, and an unset
`spec.metadataWorkers` renders `metadata_workers = 4` (`DefaultMetadataWorkers`).

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `NeutronMetadataAgentSpec`, one for adding or removing the ref and one for renaming it |
| `chassisRef is immutable` | A third transition rule on the same spec. An agent re-pointed at another chassis lands on another set of nodes, whose local Open vSwitch databases carry none of the ports it was answering for |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec` on `spec.image` |
| `exactly one of clusterRef or secretRef must be set` | Inherited from `commonv1.MessagingSpec` on `spec.messaging` |
| `caBundleSecretRef requires protocol https` | A CEL rule on `NovaMetadataSpec`, `!has(self.caBundleSecretRef) \|\| (has(self.protocol) && self.protocol == 'https')`. The agent verifies the Nova metadata API's certificate only over TLS, so a bundle beside `http` would be mounted and never read |

The three transition rules are evaluated on UPDATE only. Beside them the schema
carries the ordinary field markers: the `^\d{4}\.[12]$` pattern on
`spec.openStackRelease`, `MinLength=1` on `spec.chassisRef.name`, the
`Minimum=1` / `Maximum=65535` pair on `spec.novaMetadata.port`, the
`http`/`https` enum on `spec.novaMetadata.protocol`, and `Minimum=0` on
`spec.metadataWorkers`.

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
| `protocol must be http or https` | `spec.novaMetadata.protocol` is neither. An empty value is admitted and left to the defaulting webhook, which fills `http`, and to the renderer, which omits the key for `http` and an empty value alike; the enum marker refuses anything else at the schema layer already |
| `sharedSecretRef.name must be set when spec.novaMetadata.sharedSecretRef is configured` | The ref is present with an empty `name` |
| `caBundleSecretRef.name must be set when spec.novaMetadata.caBundleSecretRef is configured` | The CA ref is present with an empty `name` |
| `caBundleSecretRef requires protocol https: the agent verifies the Nova metadata API's certificate only over TLS` | The CA ref is present and `spec.novaMetadata.protocol` is not `https`, including an empty protocol. The webhook-layer twin of the CEL rule |
| `metadataWorkers must be non-negative` | `spec.metadataWorkers` is set below 0, mirroring the `Minimum=0` marker |

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

The release floor, which has no schema counterpart. The CRD pattern admits any
release of the `YYYY.N` cadence, and the floor moves with the operator version:

| Message | Trigger |
| --- | --- |
| `must be 2026.1 or later: this operator version no longer supports OpenStack releases below 2026.1` | `spec.openStackRelease` names a release below `2026.1`, the oldest this operator version supports, on create or when an update changes it. A change from one release below the floor to another is rejected too |

An update that keeps a stored value below the floor is admitted with a warning,
so an unrelated edit never blocks the resource:

```text
spec.openStackRelease %q is below 2026.1, the oldest OpenStack release this operator version supports; the unchanged value is admitted, but the operator renders the 2026.1 configuration for it. Set spec.openStackRelease to 2026.1 or later.
```

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
| `[DEFAULT] metadata_proxy_shared_secret` | `spec.novaMetadata.sharedSecretRef` | The shared secret is env-injected from the referenced Secret, so a file override is ignored at runtime and copies credential material into the rendered ConfigMap |
| `[ovs] ovsdb_connection` | `spec.chassisRef` | The agent reads the local Open vSwitch database over the socket the chassis pods share; another address points it at a node whose ports it is not answering for |
| `[ovn] ovn_sb_connection` | `spec.chassisRef` | The connection string is the Southbound address the `OVNCentral` the referenced chassis registers with publishes for the chassis's cluster: the internal one on the central's cluster, the node-port one from another; another address points the agent at a logical model it does not serve |
| `[ovn] ovn_sb_private_key` | operator-computed | The operator mounts the client keypair the chassis publishes in `status.clientSecretName`, signed by the `OVNCentral` issuer; another path names a file the pod does not carry, and the connection falls back to no client identity |
| `[ovn] ovn_sb_certificate` | operator-computed | The certificate half of the same keypair |
| `[ovn] ovn_sb_ca_cert` | operator-computed | The CA bundle verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision |

Every other entry in the registry is honored and reported: `[DEFAULT]`
`state_path`, `debug`, `nova_metadata_host`, `nova_metadata_port`,
`nova_metadata_protocol`, `auth_ca_cert` and `metadata_workers`,
`[agent] root_helper`, the `[oslo_messaging_notifications] driver`, the five
`[oslo_messaging_rabbit]` keys, and `[oslo_concurrency] lock_path`. The broker
keys are registered unconditionally although they render only while
`spec.messaging` is set: the registry records that a key is not the user's to
set, not that it is currently rendered.

`nova_metadata_protocol` joined the registry with `spec.novaMetadata.protocol`.
Before that `spec.extraConfig` was the only way to set it, so an agent that sets
it there reports `ExtraConfigHealthy=False` with an `ExtraConfigOwnedKeyOverride`
Warning event after the upgrade, although its rendered config does not change.
Move the value to `spec.novaMetadata.protocol` and drop the entry from
`spec.extraConfig`.

`auth_ca_cert` joined the registry the same way, with
`spec.novaMetadata.caBundleSecretRef`. Before that an `extraConfig` value named
a file the pod does not carry. An agent that sets it there keeps its value,
because `extraConfig` is merged last, and reports `ExtraConfigHealthy=False`
with an `ExtraConfigOwnedKeyOverride` Warning event. Move the bundle into a
Secret, name it in `spec.novaMetadata.caBundleSecretRef` and drop the entry
from `spec.extraConfig`. `nova_metadata_insecure` has no typed field and stays
an ordinary `spec.extraConfig` key.

`metadata_workers` joined the registry the same way, with
`spec.metadataWorkers`. An agent that sets it in `spec.extraConfig` keeps its
value, because `extraConfig` is merged last, and reports
`ExtraConfigHealthy=False` naming `[DEFAULT] metadata_workers`. Move the value to
`spec.metadataWorkers` and drop the entry from `spec.extraConfig` to clear the
condition.

`[agent] root_helper` joined the registry with the operator default `env` and
has no typed field. An agent whose `spec.extraConfig` sets no `root_helper` in
`[agent]` gets a new config ConfigMap at the operator upgrade, and its
DaemonSet rolls once on every node. An agent that carries `root_helper: env`
there renders the same bytes and does not roll. It reports
`ExtraConfigHealthy=False` with an `ExtraConfigOwnedKeyOverride` Warning event
until the entry is removed from `spec.extraConfig`, and removing it does not
roll the DaemonSet either. Any other value, for a custom image that needs
another helper, is honored and reported the same way.

Keep the `root_helper: env` entry until a rollback to an operator release
without this default is ruled out. That release renders no `root_helper`, so
the agent falls back to `sudo`, finds no `privsep-helper` and provisions no
network. While the entry is set, a rollback still renders `root_helper = env`.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `installedImage` | `string` | The image reference the running DaemonSet was projected from, recorded once the DaemonSet reports a ready pod on every node it schedules. A rollout that has reached no node leaves the previous value in place, which is what tells the two apart |
| `desiredNumberScheduled` | `int32` | How many nodes the DaemonSet should run on, mirrored from the DaemonSet status |
| `numberReady` | `int32` | How many of those nodes have a ready agent pod |

### Conditions

Four sub-reconcilers each own one condition type, and every one of the four is
set on every pass: the agent runs no optional step. The aggregate `Ready` is
`True` only when all four are. For the pipeline that sets them see
[Reconciler Architecture](./neutron-reconciler.md).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `ChassisReady` | True | `ChassisResolved` | The `OVNChassis` resolved, its `OVNCentral` published the Southbound address for the chassis's cluster, and the chassis published the client Secret its pods mount. The message names the chassis, the central and the address |
| `ChassisReady` | False | `ChassisNotFound` | No `OVNChassis` of that name in this CR's namespace. An agent applied before its chassis is an ordinary ordering of two objects in one manifest, so this polls |
| `ChassisReady` | False | `ChassisReadError` | Reading the `OVNChassis` failed |
| `ChassisReady` | False | `ChassisOnAnotherCluster` | The two CRs name different target clusters. The agent shares the chassis's nodes and mounts the Secret the chassis publishes, and neither a node nor a Secret crosses a cluster boundary. No requeue: both refs are immutable, so only deleting and reapplying one of the two can repair it |
| `ChassisReady` | False | `ChassisNotReady` | The chassis has not published `status.clientSecretName` yet, and nothing stands in for it. It is set by the chassis's client-Secret step. On the central's cluster the central's own `status.clientSecretName` stands in, so only a chassis on another cluster, or a central that has not named its Secret either, leaves the agent here. Polls |
| `ChassisReady` | False | `CentralNotFound` | The `OVNCentral` the chassis attaches to does not exist in this namespace |
| `ChassisReady` | False | `CentralReadError` | Reading the `OVNCentral` failed |
| `ChassisReady` | False | `CentralNotExternallyReachable` | The central projects onto another cluster than the chassis and does not set `spec.southbound.externallyReachable`. The message names both clusters and the field. No requeue: the fix is an edit to the central |
| `ChassisReady` | False | `CentralNotReady` | The central has published no Southbound address for the chassis's cluster yet: the internal one on the same cluster, the one outside its cluster otherwise |
| `ChassisReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve. This is the pipeline's first gate, so the failure lands on the condition the rest of the graph waits behind. A deleting CR waiting for its target cluster to come back reports the same reason here |
| `SecretsReady` | True | `SecretsAvailable` | Every credential the agent needs is there. A CR that sets none of `spec.novaMetadata.sharedSecretRef`, `spec.novaMetadata.caBundleSecretRef` and `spec.messaging` reaches this without reading anything |
| `SecretsReady` | False | `WaitingForNovaSharedSecret` | The Secret named by `spec.novaMetadata.sharedSecretRef` is missing, or carries no value under the configured key. The message distinguishes a missing ExternalSecret from one that has not synced and from a Secret missing keys |
| `SecretsReady` | False | `WaitingForNovaMetadataCA` | The Secret named by `spec.novaMetadata.caBundleSecretRef` is missing, or carries no value under the configured key, which the message names. It is gated after the shared secret. The Config and DaemonSet steps wait, so no pod is started with a volume that never mounts |
| `SecretsReady` | False | `WaitingForMessagingCredentials` | The managed `RabbitmqCluster` has published no default-user Secret yet, or the brownfield Secret carries no transport URL |
| `SecretsReady` | False | `ConfigError` | Rendering or pruning the config ConfigMap failed. Config artefacts gate the same downstream step as the credentials, so they share this condition and there is no separate one for them |
| `DaemonSetReady` | True | `DaemonSetReady` | The DaemonSet runs a ready pod on every node it schedules. The message counts the nodes |
| `DaemonSetReady` | False | `DaemonSetProgressing` | Fewer nodes run a ready pod than the DaemonSet schedules. The message counts both |
| `DaemonSetReady` | False | `DaemonSetError` | The DaemonSet could not be applied or read |
| `Ready` | True | `AllReady` | All four sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |
| `VPAReady` | True | `VPAReady` | The VerticalPodAutoscaler of every opted-in workload is applied; the message names them. See [VerticalAutoscalingSpec](../keystone/keystone-crd.md#vpaready-condition) |
| `VPAReady` | True | `VPANotRequired` | No workload opts in (`spec.verticalAutoscaling` unset); a VPA the CR created before is deleted |
| `VPAReady` | False | `VPANotInstalled` | A workload opts in, but the cluster the children land on does not serve `autoscaling.k8s.io/v1` `VerticalPodAutoscaler`. Nothing is created, and the other conditions still converge |
| `VPAReady` | False | `CapabilityProbeFailed` | The target cluster the CR names could not be probed for the kind |
| `VPAReady` | False | `VPAError` | Listing, applying or deleting a VPA failed; the message carries the error |

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

The figure shows the agent's pod beside the three other pods of such a node.
Its init container is the only one that waits for the chassis to be registered
in the Southbound database.

![One hypervisor node with the four pods three resources put on it. An OVNChassis creates two DaemonSets: the pod {chassis}-ovs with the init container host-prepare and the containers ovsdb-server and ovs-vswitchd, and the pod {chassis}-ovn-controller with the init container apply-node and the container ovn-controller. A NeutronMetadataAgent creates the pod {agent}-metadata-agent, a NovaCompute the pod {pool}-nova-compute. All four run in the network namespace of the node, beside a libvirtd that no operator runs. Three gates order the start. The operator creates the ovn-controller DaemonSet only once every OVS pod is Ready. The init container wait-for-chassis of nova-compute waits until apply-node has written a system-id into the local Open vSwitch database. The init container wait-for-chassis of the metadata agent waits until ovn-controller has registered the chassis in the Southbound database. The pods share host paths: /run/openvswitch is mounted by all four, /run/ovn by the two chassis pods, /run/netns by the metadata agent, /run/libvirt and /var/lib/nova by nova-compute and libvirtd. A gateway node has no further pod: it also matches spec.gateway.nodeSelector, and apply-node sets ovn-cms-options=enable-chassis-as-gw.](../../diagrams/compute-node-anatomy.svg)

The pod runs with `hostNetwork: true`: it answers the 169.254.169.254 requests
arriving on the node's own interfaces. Two host paths are mounted, both
`DirectoryOrCreate` so a node that has never run Open vSwitch still starts:

| Path | Mount | Why it is a host path |
| --- | --- | --- |
| `/run/openvswitch` | read-write, on the init container and the agent container | The local Open vSwitch database socket the chassis pods create. It is what the agent reads its node's port bindings from, and an emptyDir would leave it reading nothing |
| `/run/netns` | read-write with `Bidirectional` mount propagation | The network namespaces the agent creates. Bidirectional propagation is what makes them visible to the node, which is how the datapath reaches the proxies running inside them |

With `spec.novaMetadata.caBundleSecretRef` set, the agent container mounts the
named Secret read-only at `/etc/nova-metadata-ca`, mode `0444`, projecting the
configured key as `ca.crt`. The init container sends no request to the Nova
metadata API and does not get this mount. The path lies outside the read-only
`/etc/neutron` config mount. Neutron opens the file on every proxied request
and the volume has no `subPath`, so a rotated bundle reaches the running pods
without a rollout.

The agent container runs privileged and pinned to uid 0, with
`runAsNonRoot: false`. It creates network namespaces, moves interfaces into them
and starts a haproxy per network through privsep, which the Restricted profile
denies and no named capability covers. The operator renders
`[agent] root_helper = env`, so the agent starts privsep-helper as the
container's own user, and privsep-helper needs uid 0 for the capabilities it
keeps. The image's own unprivileged user does not work either. The pod-level
security context carries the seccomp profile and nothing else: no `fsGroup`,
because it would be applied to the host directories the pod mounts, where the
ownership is the node's business. The `wait-for-chassis` init container runs
under the Restricted profile.

That init container is the same-node gate. The agent reads
`external_ids:system-id` from the local Open vSwitch database once, at start,
and takes it as its chassis name. It then writes its registration into the
`Chassis_Private` row of that name in the Southbound database and retries for
as long as the row is missing, with its proxy socket open and the pod `Ready`.
The chassis's `host-prepare` init container creates `/run/openvswitch/conf.db`
only when the file is missing, so on a node whose database outlived an earlier
OVNChassis the `system-id` is that chassis's old id until the new chassis's
`apply-node` init container writes the new one. An agent started in between
never registers, and the servers on its node get no metadata.

The gate waits for the registration the agent depends on. Every 2 seconds it
reads the local `system-id` again and decides:

| State | Log line | Outcome |
| --- | --- | --- |
| No UUID-shaped `system-id` in the local database (socket missing, query failed, key absent, value not a UUID) | `waiting for the chassis to write its system-id into the local Open vSwitch database` | wait |
| A `system-id` without a `Chassis_Private` row of that name (the id is stale, ovn-controller has not registered it yet, or the Southbound query failed) | `waiting for chassis <id> to register in the Southbound database` | wait |
| A `system-id` and its `Chassis_Private` row | `chassis <id> is registered in the Southbound database` | exit 0 |

On a reused database the gate waits in the second state on the old id and
passes on the new one. It prints a message only when the message changes, so a
pod held in `Init:0/1` logs the id it waits for without a line every 2 seconds.
While a pod is held there, the DaemonSet step reports `DaemonSetReady=False`
with reason `DaemonSetProgressing`. A Southbound fault that persists (a wrong
address, an unreadable key, a rejected certificate) logs the same second line,
because the gate discards the errors of its queries.

The init container carries `OVN_SB_CONNECTION`, the Southbound address the
agent's `[ovn] ovn_sb_connection` gets, and mounts the `ovn-tls` client Secret
read-only at `/etc/ovn/tls` beside `/run/openvswitch`. It queries the
Southbound database with `--no-leader-only`, as the agent's own connection
does, so a leader election does not hold it. An empty `OVN_SB_CONNECTION` is a
rendering fault no wait repairs: the gate prints `OVN_SB_CONNECTION is not set`
on stderr and exits 1.

Both workloads select the same nodes and nothing orders the two DaemonSets, so
the gate is per node rather than per cluster. The queries go through
`ovsdb-client` because the neutron image ships the OVS Python client without
the `ovs-vsctl` binary.

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
  openStackRelease: "2026.1"
  image:
    repository: ghcr.io/c5c3/neutron
    tag: "2026.1"
  chassisRef:
    name: neutron-agent-chassis
```

This is the fixture the `metadata-agent` suite applies
(`tests/e2e/neutron/metadata-agent/03-neutronmetadataagent-cr.yaml`). It leaves
`spec.messaging` out, which is what gives the suite's "no `transport_url` line"
assertion something to check, and `spec.novaMetadata` too, since Nova is not
onboarded onto this operator. Without either block the CR references no Secret
at all, the path `SecretsReady=True` with reason `SecretsAvailable` covers.
