---
title: Neutron Operator
quadrant: operator
---

# Neutron Operator

The Neutron operator runs the OpenStack network service on one mechanism
driver. `mechanism_drivers = ovn` is what the rendered `ml2_conf.ini` carries,
and no other value is reachable through the CRD. There is no ML2/OVS stack under
it and no agent tier beside it: no L2, L3 or DHCP agent is projected, no
`neutron-dynamic-routing`, and no BGP agent. The logical network model lives in
the OVN Northbound database the [OVN operator](../ovn/index.md) runs, and this
operator programs it through the ML2/OVN mechanism driver.

## Processes

Three long-running processes carry the service, plus the per-node agent:

| Process | Workload | What it does |
| --- | --- | --- |
| uWSGI on `neutron.wsgi.api` | the `{name}` Deployment of a `Neutron` | The REST API on port 9696. The image ships no entry script, so uWSGI loads the module directly, and the configuration reaches it through `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES` |
| `neutron-periodic-workers` | the `{name}-periodic-workers` Deployment | The recurring maintenance tasks of the ML2 plugin |
| `neutron-ovn-maintenance-worker` | the `{name}-ovn-maintenance-worker` Deployment | Reconciles the Northbound model against the Neutron database |
| `neutron-ovn-metadata-agent` | the `{name}-metadata-agent` DaemonSet of a `NeutronMetadataAgent` | Answers the 169.254.169.254 requests of the instances on its node |

`neutron-rpc-server` is absent from that list. Nothing in this deployment
consumes RPC, so the process has no work, and at `rpc_workers = 0` it exits 0 on
2026.1 while crashing on 2025.2 with `AttributeError: 'NoneType' object has no
attribute 'run'` (the `oslo.service` pin, not neutron). Both worker Deployments
serve no HTTP and get no Service, no HorizontalPodAutoscaler and no
PodDisruptionBudget.

## The bus

Neutron never touches the broker in this posture. `rpc_workers` and
`rpc_state_report_workers` are both `0`, `[oslo_messaging_notifications] driver`
is `noop`, and `dhcp_agent_notification` together with the two
`notify_nova_on_port_*` options stays `false`. Measured across four states,
including after a network create, that adds up to zero connections, zero
channels, zero queues and zero non-default exchanges.

A valid `transport_url` is still required. `config.init` calls `n_rpc.init`
unconditionally in every neutron process, so the URL has to parse even though
nothing dials the broker with it. `spec.messaging` is therefore a required field
on `Neutron`, and the URL reaches every process and every migration Job as the
`OS_DEFAULT__TRANSPORT_URL` environment override sourced from the derived
`{name}-transport-url` Secret. It is never written into the rendered
configuration, so the broker password stays out of the ConfigMap.

## The two kinds

`Neutron` describes the service: the API Deployment, the two worker Deployments,
the database and cache connections, the Keystone integration, the bus, and the
optional recurring comparison of the Neutron database against the OVN Northbound
database. It names one `OVNCentral` through `spec.ovn.centralRef`, reads the two
connection strings and the client certificate off that CR's status, and mirrors
the certificate into a Secret of its own.

`NeutronMetadataAgent` describes the per-node metadata proxy. It names an
`OVNChassis` and puts one DaemonSet onto the nodes that chassis selects. The
kind is separate because it belongs in the chassis's namespace: the pods are
privileged, run on the compute and network nodes, and mount the local Open
vSwitch socket the chassis pods create. That namespace has to be listed under
`privilegedNamespaces` in the target-cluster access chart, which the API
Deployment's namespace does not. See
[Target Clusters](../target-clusters.md).

## Design decisions

The Phase-0 decision record lives in meta issue
[#898](https://github.com/C5C3/cobaltcore/issues/898). These are the entries
this operator implements.

- D2 (operator split): a separate ovn-operator with the kinds `OVNCentral` and
  `OVNChassis`, beside a neutron-operator that owns the `Neutron` kind. The
  coupling is CR status alone: this operator watches `OVNCentral` and reads the
  addresses it publishes, which keeps the OVN layer available to a later
  consumer. D7 added the second kind on this side of the split.
- D5 (RabbitMQ scope): the shared bus from
  [#895](https://github.com/C5C3/cobaltcore/issues/895), with quorum queues and
  stable queue names, `transport_url` delivered by environment override only,
  and the notifications driver at `noop`.
- D7 (metadata agent): `neutron-ovn-metadata-agent` as a DaemonSet on the
  compute nodes, projected by this operator rather than by the ovn-operator,
  because this operator is the one that renders
  `neutron_ovn_metadata_agent.ini`. It carries a same-node startup gate on
  chassis readiness, expressed as an init container that waits on the local
  ovsdb socket.
- D10 (neutron-server launch mode): one launch mode for the API on both
  releases. `neutron/wsgi/api.py` is byte-identical at 27.0.3 and 28.0.1 and
  neither release ships a `neutron-server` binary, so the API is
  `uwsgi … --module neutron.wsgi.api`. Configuration travels in
  `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES`, since `--config-dir`
  alone fails with `ConfigFilesNotFoundError`. `--set-placeholder
  start-time=%t` must not be rendered on the command line: uWSGI passes the
  literal `%t` and neutron dies on `int('%t')`. The operator writes
  `start-time = %t` into a `uwsgi.ini` instead, where uWSGI expands it while
  reading the file.
- D11 (minimal RPC-worker posture): the process inventory above and the bus
  posture above. The contrast run with the two worker keys removed cost one AMQP
  connection, one channel, four durable quorum queues and three exchanges, and
  those queues outlive the process that declared them.

BGP is out of scope here and tracked in its own meta,
[#899](https://github.com/C5C3/cobaltcore/issues/899).

## Owned resources

For a `Neutron` named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The API pods, uWSGI on port 9696 |
| Service | `{name}` | ClusterIP in front of the API pods on port 9696 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` above one replica, `maxUnavailable: 1` at one; selects the API component only |
| HorizontalPodAutoscaler | `{name}` | Only while `spec.autoscaling` is set; the workers are never autoscaled |
| NetworkPolicy | `{name}` | Only while `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only while `spec.gateway` is set |
| MariaDB `Database` / `User` / `Grant` | `{name}` | Managed mode only (`spec.database.clusterRef`); a brownfield database is left alone |
| Deployment | `{name}-periodic-workers` | `neutron-periodic-workers` |
| Deployment | `{name}-ovn-maintenance-worker` | `neutron-ovn-maintenance-worker` |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `neutron.conf`, `ml2_conf.ini` and `uwsgi.ini`, plus `logging.conf` for json logging; 3 historical retained |
| Secret | `{name}-db-connection` | The derived pymysql DSN, consumed through `OS_DATABASE__CONNECTION` |
| Secret | `{name}-transport-url` | The derived `rabbit://` URL, consumed through `OS_DEFAULT__TRANSPORT_URL` |
| Secret | `{name}-ovn-client` | The mirror of the client identity the `OVNCentral` publishes: `tls.crt`, `tls.key`, `ca.crt` |
| Job | `{name}-db-sync` | `neutron-db-manage upgrade head` |
| Job | `{name}-db-expand`, `{name}-db-migrate`, `{name}-db-contract` | The three release-upgrade phases |
| CronJob | `{name}-ovn-db-sync` | `neutron-ovn-db-sync-util`, only while `spec.ovnDBSync` is set |

For a `NeutronMetadataAgent` named `{name}`:

| Resource | Name | Purpose |
| --- | --- | --- |
| DaemonSet | `{name}-metadata-agent` | The agent pods on the nodes the referenced `OVNChassis` selects |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `neutron_ovn_metadata_agent.ini`, plus `logging.conf` for json logging; 3 historical retained |
| Secret | `{name}-transport-url` | The derived `rabbit://` URL, written only while `spec.messaging` is set |

The client Secret the agent pods mount is not in that list. cert-manager issues
it and the `OVNCentral` publishes it; this operator reads its name off the
central and mounts it.

## Reference pages

- [Neutron CRD](./neutron-crd.md): the `spec`/`status` contract of the service,
  the rendered defaults, and the validation rules
- [NeutronMetadataAgent CRD](./neutron-metadata-agent-crd.md): the node
  contract, the agent's own spec, and its conditions
- [Neutron Controller Events](./neutron-events.md): the Kubernetes events both
  controllers emit
- [Reconciler Architecture](./neutron-reconciler.md): the two pipelines, their
  conditions, and requeue semantics

A ControlPlane projects the `Neutron` child from
[`ServiceNeutronSpec`](../c5c3/controlplane-crd.md#serviceneutronspec), which
derives the database, the cache, the Keystone endpoint and the bus from the
plane and leaves the rest to that spec. The OVN layer this operator programs is
documented under [OVN Operator](../ovn/index.md).
