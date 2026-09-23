---
title: Nova Operator
quadrant: operator
---

# Nova Operator

The Nova operator runs the OpenStack compute control plane: the compute API
under uWSGI, the metadata API, the scheduler, the conductor, and the noVNC
console proxy. One kind, `Nova`, lives in `nova.openstack.c5c3.io/v1alpha1`,
and one CR describes the whole control plane.

The compute nodes are not part of it. A `nova-compute` process runs on the
hypervisor, outside this cluster, and joins over the message bus. What the CR
publishes for it is the compute contract: a rendered `nova.conf` fragment and
the bus credentials, in the Secret `status.computeConfigSecretRef` names.

## Processes

Five long-running processes carry the service.

| Process | Workload | What it does |
| --- | --- | --- |
| uWSGI on `nova.wsgi.osapi_compute` | the `{name}` Deployment | The compute API on port 8774 |
| uWSGI on `nova.wsgi.metadata` | the `{name}-metadata` Deployment | The metadata API on port 8775, which the Neutron metadata agent proxies an instance's call to 169.254.169.254 to |
| `nova-scheduler` | the `{name}-scheduler` Deployment | Picks a host for every instance the conductor asks it about |
| `nova-conductor` | the `{name}-conductor` Deployment | The one process that reaches the cell database on behalf of a compute node |
| `nova-novncproxy` | the `{name}-novncproxy` Deployment | Bridges a browser's noVNC session to the VNC server of the hypervisor an instance runs on, on port 6080 |

No `[DEFAULT] host` is rendered, so every replica registers under its own pod
name. That is what lets the scheduler and the conductor run more than one pod:
each holds a service record of its own, while a shared identity would collapse
the fleet into a single row. It is also what picks the one scheduler that runs
the periodic host discovery, `[scheduler] discover_hosts_in_cells_interval`
(300 seconds), the pass that maps a newly registered compute node into the cell.

The scheduler and the conductor serve no HTTP port. Their readiness probe is
`nova-amqp-ready`, which reports whether a process of the container holds a
socket established to the broker port (see
[Container Images](../ci-cd/container-images.md#nova)). The console proxy is
probed with a GET of `/vnc_lite.html`, the client page it serves itself.

## Design decisions

The decision record lives in meta issue
[#1014](https://github.com/C5C3/cobaltcore/issues/1014). These are the entries
this operator implements, together with the choices the CRD carries.

- Two database blocks, never one. `spec.apiDatabase` holds the global tables
  (the cell map, the flavors, the instance mappings) and `spec.database` the
  per-cell instance state. Each runs its own migrations, so a CR pointing both
  at one schema would install two migration histories into it: a CEL rule
  refuses it, and a second rule keeps the two on the same `credentialsMode`.
- The cells are mapped by the `{name}-db-sync` Job, from templates rather than
  from assembled URLs. `cell_v2 map_cell0` and `cell_v2 create_cell` are handed
  `{scheme}://{username}:{password}@{hostname}:{port}/…` placeholders that nova
  expands from the connection it already runs with, so the operator never
  assembles a URL carrying a password. `create_cell` is guarded by an `awk`
  over `cell_v2 list_cells` that looks for a cell named `cell1` or one mapped
  onto the cell schema: a second `create_cell` neither fails nor updates the
  existing row, it maps a second cell onto the same schema, and from then on an
  instance boots into whichever of the two the scheduler was handed.
- One service user, five client sections. `spec.serviceUser` authenticates the
  token middleware and every outgoing call, and `[placement]`, `[neutron]` and
  `[cinder]` each carry a password of their own through an environment
  override. `[glance]` and `[barbican]` carry none: nova reads an image and a
  volume-encryption key with the token of the request it is serving, and
  `[service_user] send_service_user_token = true` sends nova's own token
  alongside it, so a long boot outlives the user token's expiry.
- The compute contract is a Secret, not a CRD. `{name}-compute-config` carries
  the `nova.conf` fragment a compute node reads, the transport URL, the
  service-user password, the metadata shared secret and the cell name. There is
  no compute kind in this API group, because a `nova-compute` runs on a
  hypervisor this operator does not schedule onto.
- The console proxy takes a hostname of its own. The console URL the API hands
  a browser is `https://<host>/vnc_lite.html?path=%3Ftoken%3D<token>`, so the
  page and the WebSocket that follows it both open on `/` with nothing but a
  query string to tell them apart. A path prefix under the API's hostname
  cannot separate them, which is why `spec.consoleProxy.gateway` is a third
  gateway block instead of a path on the first.
- Instance lifecycle notifications are dropped at the source
  (`[oslo_messaging_notifications] driver = noop`) on the control plane and in
  the compute fragment alike, because nothing in this deployment consumes them.
- The metadata shared secret never reaches the rendered config. It arrives at
  the metadata pods as `OS_NEUTRON__METADATA_PROXY_SHARED_SECRET`, sourced from
  `spec.metadata.sharedSecretRef`, and `[neutron] metadata_proxy_shared_secret`
  is rejected in `spec.extraConfig`: a file value is inert at runtime and would
  only copy the value a proxied request's signature is verified with into the
  ConfigMap every pod mounts.

## Owned resources

For a `Nova` named `{name}`:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The API pods, uWSGI on port 8774 |
| Service | `{name}` | ClusterIP in front of the API pods on port 8774 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` above one replica, `maxUnavailable: 1` at one; selects the API component and excludes Job pods |
| HorizontalPodAutoscaler | `{name}` | Only while `spec.autoscaling` is set; the API is the only autoscaled Deployment |
| NetworkPolicy | `{name}` | Only while `spec.networkPolicy` is set; one policy covers all five workloads and the Job pods |
| NetworkPolicy | `{name}-novncproxy` | Only while `spec.networkPolicy` is set and the console proxy is enabled; the proxy's egress to the hypervisors' VNC ports |
| HTTPRoute | `{name}`, `{name}-metadata`, `{name}-console` | One per gateway block that is set |
| Deployment / Service | `{name}-metadata` | The metadata API on port 8775 |
| Deployment | `{name}-scheduler`, `{name}-conductor` | The two bus processes |
| Deployment / Service | `{name}-novncproxy` | The console proxy on port 6080, only while it is enabled |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `nova.conf` and the four role overlays, plus `logging.ini` under json logging; 3 historical retained |
| Secret | `{name}-api-db-connection`, `{name}-db-connection` | The two derived pymysql DSNs |
| Secret | `{name}-transport-url` | The derived `rabbit://` URL |
| Secret | `{name}-compute-config` | The compute contract, updated in place under a stable name |
| Job | `{name}-db-sync` | Both schema migrations and the cell mapping |
| Job | `{name}-db-expand`, `{name}-db-migrate`, `{name}-db-contract` | The three release-upgrade phases |
| CronJob | `{name}-db-archive` | The recurring `nova-manage db archive_deleted_rows` |
| MariaDB `Database` / `User` / `Grant` | `{name}-api`, `{name}` | Managed mode only (`spec.apiDatabase.clusterRef` / `spec.database.clusterRef`); a brownfield database is left alone |
| MariaDB `Database` / `Grant` | `{name}-{database}-cell0` | cell0, an additional schema on the cell block's user |

## Reference pages

- [Nova CRD](./nova-crd.md): the `spec`/`status` contract, the rendered
  configuration, the compute contract, and the validation rules
- [Controller Events](./nova-events.md): the Kubernetes events the controller
  emits
- [Reconciler Architecture](./nova-reconciler.md): the sub-reconciler pipeline,
  conditions, and requeue semantics
- [Upgrade Flow](./nova-upgrade-flow.md): the expand-migrate-contract phases,
  the pre-flight exit codes, the cell0 contract pass, and the abort recipe
- [Cells](./nova-cells.md): cell0 and cell1, the template URLs, host discovery
  and unmapping, and why one CR maps a single real cell
