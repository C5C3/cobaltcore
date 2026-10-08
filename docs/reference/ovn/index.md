---
title: OVN Operator
quadrant: operator
---

# OVN Operator

The OVN operator runs the SDN layer the Neutron ML2/OVN mechanism driver
programs. It deploys no OpenStack service of its own: what it manages is OVN and
Open vSwitch, across two kinds.

`OVNCentral` describes the control plane as one unit. The operator projects the
Northbound and Southbound Raft databases as two StatefulSets with a headless
Service and one Service per member, the `ovn-northd` Deployment that compiles
the Northbound model into Southbound flows, an optional Southbound relay tier,
the cert-manager Certificates every OVN connection is authenticated with, and
the CronJob that snapshots both databases. The two databases share one CR
because northd only works against both of them and their Raft clusters have to
be sized together.

`OVNChassis` describes the node layer. The operator projects the `ovs` and
`ovn-controller` DaemonSets onto the nodes a label selector picks, plus the
per-node Jobs that apply a node's `external_ids`, evacuate a gateway node, and
remove its chassis row from the Southbound database.

The two kinds are coupled through status. An `OVNChassis` names an `OVNCentral`,
waits for its published Southbound address and for `status.clientSecretName`,
and mounts that Secret. When the two project onto different clusters, the
chassis reads the addresses the central publishes on node ports and copies the
Secret onto its own cluster, as `<chassis>-ovn-client`. That link is the whole
coupling.

The figure shows what one `OVNCentral` runs and which client talks to which
database. Only `ovn-controller` is pointed at the relay; every other client
dials the database members.

![What one OVNCentral runs and who talks to it. The Northbound database {central}-nb and the Southbound database {central}-sb are Raft clusters of three members each by default. northd reads the Northbound database and writes the flows into the Southbound database. A relay in front of the Southbound database exists only with spec.relay. A CronJob backs both databases up, and cert-manager issues a server certificate per database, one for the relay and one client certificate from the ClusterIssuer that spec.tls.issuerRef names. On the Neutron side the API with its ML2/OVN driver, the maintenance worker and the periodic workers hold both connections, and the optional CronJob {neutron}-ovn-db-sync compares the Northbound database with the Neutron database or rewrites it. On every chassis node ovn-controller registers the chassis in the Southbound database and reads its flows there, and the metadata agent watches the Southbound database and writes its Chassis_Private row. Two Jobs of an OVNChassis write as well: the evacuation removes gateway bindings from the Northbound database, and the chassis deletion removes the chassis row from the Southbound database. Only ovn-controller is pointed at the relay; every other client dials the database members.](../../diagrams/compute-ovn-control-plane.svg)

## Design decisions

The Phase-0 decision record lives in meta issue
[#898](https://github.com/C5C3/cobaltcore/issues/898). These are the entries
this operator implements.

- D1 (OVN source and version line): self-built images from pinned `ovn-org/ovn`
  sources, with Open vSwitch at OVN's own submodule pin, on the OVN 26.03 LTS
  line. The operator resolves `ghcr.io/c5c3/ovn` at that version when a CR
  leaves `spec.image` unset.
- D2 (operator split): one ovn-operator with the kinds `OVNCentral` and
  `OVNChassis`, beside a neutron-operator that owns the `Neutron` kind. The
  coupling between them is CR status: the neutron-operator watches `OVNCentral`
  and reads the addresses it publishes. That keeps the OVN layer available to a
  later consumer.
- D3 (OVN central placement): the central runs on the same cluster as the
  chassis, which keeps the Southbound read path local, and publishes two
  addresses per database. `status.<db>.internalDbAddress` is for consumers on
  that cluster, `status.<db>.dbAddress` for a management-side neutron-server and
  for a chassis on another cluster, which also dials a relay published outside
  the central's cluster (`status.relayDbAddress`).
- D4 (chassis management): OVS and ovn-controller run as DaemonSets projected
  from `OVNChassis`, steered by node labels, and an `OVNChassis` works on its own
  without a Neutron above it. Host-native OVS is revisited only if DPDK or
  SR-IOV becomes a requirement.
- D6 (BGP path): flat and L2 provider networks now, with native OVN dynamic
  routing and the Neutron `ovn-bgp` service plugin as the target; ovn-bgp-agent
  is skipped. This operator has no BGP surface. What it keeps open is the OVN
  26.03 line from D1 and gateway-chassis selection by node label.
- D9 (cross-cluster NB/SB exposure): each Raft member gets its own `NodePort`
  Service under `spec.<db>.externallyReachable`, and both published addresses
  carry IP literals. python-ovs resolves no name without the `unbound` library,
  which no release pins, and the ovsdb C tools ignore the pod's DNS search list.
  The same lab found that python-ovs accepts any certificate the configured CA
  signed without checking the hostname, which is why the OVN CA is scoped to
  OVN: every certificate it signs is a valid client of both databases.
- D12 (recurring maintenance inventory): a backup CronJob for both databases,
  with a daily schedule, a retention bound and a suspend switch. The lab that
  settled it also pinned the restore constraints: `ovsdb-client restore` needs a
  seekable stdin, so a snapshot is staged as a file; a clustered snapshot
  restored into a standalone server needs `--force`; and a failed run leaves a
  zero-byte file behind, so retention gates on the exit code.

BGP itself is tracked in its own meta,
[#899](https://github.com/C5C3/cobaltcore/issues/899).

## Owned resources

The figure under
[Owned Resources](../keystone/keystone-reconciler.md#owned-resources) of the
Keystone operator draws the baseline this list follows: serving objects, config
and Secrets, Jobs and CronJobs.

For an `OVNCentral` named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| StatefulSet | `{name}-nb`, `{name}-sb` | One Raft cluster per database, `spec.<db>.replicas` members, started in parallel, with a soft zone and hostname spread |
| PodDisruptionBudget | `{name}-nb`, `{name}-sb` | `maxUnavailable: 1` over one database's members, so a drain evicts one member at a time |
| Service (headless) | `{name}-nb`, `{name}-sb` | Raft peer discovery; publishes not-ready addresses so a fresh cluster can form |
| Service (per member) | `{name}-nb-0`, `{name}-sb-0`, one per ordinal | ClusterIP, or `NodePort` at `nodePortBase + ordinal` under `spec.<db>.externallyReachable` |
| PersistentVolumeClaim | `db-{name}-nb-0`, one per member | From the StatefulSet's `db` volume claim template; retained on scale-down, deleted with the CR |
| ConfigMap | `{name}-central-scripts` | The run, set-connection, and backup scripts both databases share |
| Deployment | `{name}-northd` | `ovn-northd`; one replica is active and the rest wait on the Southbound lock |
| Deployment | `{name}-sb-relay` | Only while `spec.relay` is set |
| Service | `{name}-sb-relay` | ClusterIP in front of the relays, published as `status.relayAddress`. `NodePort` under `spec.relay.externallyReachable`, then also published as `status.relayDbAddress` |
| Certificate + Secret | `{name}-nb-server`, `{name}-sb-server`, `{name}-client`, and `{name}-sb-relay` with a relay | cert-manager issues them; each Secret takes its Certificate's name |
| PersistentVolumeClaim | `{name}-backup` | The snapshot volume |
| CronJob | `{name}-backup` | Snapshots both databases and prunes the window |

For an `OVNChassis` named `{name}`:

| Resource | Name | Purpose |
| --- | --- | --- |
| DaemonSet | `{name}-ovs` | The `ovsdb-server` and `ovs-vswitchd` containers on the selected nodes, in the node's network namespace |
| DaemonSet | `{name}-ovn-controller` | `ovn-controller`, connected to the relay address the central published, or to its Southbound address when it runs no relay the chassis can reach |
| ConfigMap | `{name}-nodes` | One key per selected node, carrying that node's `external_ids` values |
| ConfigMap | `{name}-chassis-scripts` | The scripts both DaemonSets and the maintenance Jobs run |
| Job | `{name}-apply-<hash>`, `{name}-evacuate-<hash>`, `{name}-chassis-del-<hash>` | Per node; the node name is hashed to eight hex characters because a Job name has 63 and a node name up to 253 |

## Reference pages

- [OVNCentral CRD](./ovn-central-crd.md): the `spec`/`status` contract of the
  control plane, its validation rules, and the backup
- [OVNChassis CRD](./ovn-chassis-crd.md): node selection, the per-node
  `external_ids`, and the maintenance Jobs
- [OVN Controller Events](./ovn-events.md): the Kubernetes events both
  controllers emit
- [Reconciler Architecture](./ovn-reconciler.md): the two pipelines, their
  conditions, and requeue semantics

The OVN image is release-independent and built from `images/ovn/Dockerfile`; see
[build-ovn / merge-ovn-image / verify-ovn-image](../ci-cd/build-images-workflow.md#build-ovn-merge-ovn-image-verify-ovn-image).
The `ovn` e2e leg and the multi-node overlay leg are documented in
[e2e-operator](../ci-cd/ci-workflow.md#e2e-operator) and
[e2e-ovn-overlay](../ci-cd/ci-workflow.md#e2e-ovn-overlay).
