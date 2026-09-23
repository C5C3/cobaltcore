---
title: Nova Cells
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Nova Cells

Reference documentation for the cells a `Nova` CR maps: what the two cells hold,
how their mapping rows reach a database without storing a password, how a
compute host is mapped into a cell and out of it again, and why one CR maps a
single real cell. The command sequence that writes the mappings is described
under [The cells sequence](./nova-reconciler.md#the-cells-sequence), and the two
database blocks behind them under [Design decisions](./index.md#design-decisions).

The commands on this page use the names of the
[ControlPlane quick start](../../quick-start-controlplane.md): the projected Nova
`controlplane-nova` in `openstack`.

---

## Two cells per CR

nova splits its state in two. The `nova_api` schema (`spec.apiDatabase`) holds
the global tables: the cell map, the flavors, one mapping row per instance and
one per compute host. Each cell has a schema of its own for the instances placed
in it. A `Nova` CR maps two cells:

| Cell | Schema | What it holds |
| --- | --- | --- |
| `cell0` | `{database}_cell0`, derived from `spec.database.database` | The holding pen for instances that failed scheduling. An instance no host could take is recorded here in `ERROR`, so it can still be listed and deleted |
| `cell1` | `spec.database.database` | The one real cell. Every compute host is mapped into it, and the compute contract publishes its name under `cell_name` |

Both schemas are reached with the cell block's one user, because `nova-api`
reads cell0 on every instance list. The `{name}-db-sync` Job maps the two cells
and reports their UUIDs; the eight commands it runs, and the guard that keeps a
rerun from mapping `cell1` twice, are listed under
[The cells sequence](./nova-reconciler.md#the-cells-sequence).

---

## Template URLs

A cell mapping row stores the transport URL and the database connection nova
uses to reach that cell. The operator writes both as templates:

```text
{scheme}://{username}:{password}@{hostname}:{port}/{path}?{query}
{scheme}://{username}:{password}@{hostname}:{port}/<schema>?{query}
```

A process that loads a mapping fills the placeholders from its own
`[DEFAULT] transport_url` and `[database] connection`, so the stored rows carry
no credential. Every control-plane process and Job that formats a mapping
therefore needs `[database]`, including the ones whose own work is on `nova_api`.
The console proxy sits at the other end: it gets `[database]` only, with no
`nova_api` connection, because it reads console tokens out of the cell schema.
[Environment overrides](./nova-crd.md#environment-overrides) lists which role
carries which override.

Rotation follows from the templates. A rotated database or broker credential
needs no `nova-manage cell_v2 update_cell`: the connection-hash annotations
(`nova.c5c3.io/api-db-connection-hash`, `nova.c5c3.io/db-connection-hash`,
`nova.c5c3.io/transport-url-hash`) roll every role, and the restarted processes
expand the unchanged rows with the new values.

::: warning `list_cells --verbose` prints database and broker credentials
`nova-manage cell_v2 list_cells --verbose` is documented upstream as "Show
sensitive details, such as passwords". With template mappings it prints the
expanded URLs, so the output carries the database password and the broker
password of the process it runs in. The plain `list_cells` prints the same URLs
with the passwords masked; use it, and keep `--verbose` out of scripts, tickets
and support bundles.
:::

---

## Reading the cells back

nova generates a cell's UUID when it maps the cell, and every per-cell
`nova-manage cell_v2` command addresses a cell by that UUID. The operator reads
the UUIDs off the `db-sync` Job and publishes them in `status.cells`, cell0 first:

```bash
kubectl get nova controlplane-nova -n openstack -o jsonpath='{.status.cells}'
```

The same list, read through nova itself, comes from the conductor, which carries
both database connections:

```bash
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 list_cells
```

---

## Host discovery

A `nova-compute` registers its service record in the cell database when it
starts. The scheduler places an instance only on a host that also has a host
mapping in the `nova_api` schema, and discovery is the step that writes one.

The operator renders `[scheduler] discover_hosts_in_cells_interval = 300`, so
discovery runs as a scheduler periodic every 300 seconds. With more than one
scheduler replica, the live scheduler whose host name sorts first runs it and the
others skip the pass. Every replica registers under its pod name, so that is the
lowest-named scheduler pod that reports up. A new compute is therefore mapped
within 300 seconds of registering.

A consumer that polls for the mapping adds its own interval on top.
[openstack-hypervisor-operator](https://github.com/cobaltcore-dev/openstack-hypervisor-operator)
polls every 60 seconds, so it sees a newly registered compute as mapped within
360 seconds.

A reader who does not want to wait runs the discovery by hand in the conductor
pod:

```bash
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 discover_hosts --verbose
```

`--verbose` names the hosts it mapped. Discovery is idempotent: a host that
already has its mapping is skipped. It only maps a compute that has registered,
so a run issued before the compute's service record exists maps nothing.

---

## Unmapping a host

Deleting the compute service through the API unmaps the host:

```bash
openstack compute service delete <service-id>
```

At nova 32.0.0 (the `2025.2` image) and 33.0.0 (`2026.1`) the delete also removes
the host's mapping, deletes its resource provider in Placement with a cascade,
and takes the host out of every aggregate it belongs to. It is refused with a
conflict while the host still carries instances, or while a migration involving
it is in progress. Delete or migrate those first.

A service record removed any other way leaves the mapping behind, and a compute
registering again under the same name then finds a stale row. Remove it with
`delete_host`, which requires both the cell UUID and the host name:

```bash
CELL1="$(kubectl get nova controlplane-nova -n openstack \
  -o jsonpath='{.status.cells[?(@.name=="cell1")].uuid}')"
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 delete_host \
  --cell_uuid "$CELL1" --host <host>
```

---

## Sharing a broker

The transport URL the operator derives for a managed bus always names the root
vhost `/`, the vhost the RabbitMQ Cluster Operator grants its default user. Two
Nova CRs on one managed broker therefore publish on the same `scheduler`,
`conductor` and `compute.<host>` topics, and each one's conductor answers the
other's computes.

Give each Nova a vhost of its own. Create the vhost on the broker, grant a
broker user permissions on it, store the resulting `rabbit://` URL (with an
explicit port) in a Secret, and name that Secret in `spec.messaging.secretRef`,
the brownfield form of the messaging block. The e2e suites do the same with the
helper `tests/e2e/cinder/broker-vhost.sh`.

---

## Multi-cell is a non-goal

A `Nova` CR maps cell0 and one real cell, and there is no `NovaCell` kind and no
third database block. The template URLs are the reason. nova fills every mapping
from the one `[database]` connection the process formatting it carries, so all
cells a process can reach share that process's database user. A distinct cell0
user could never be substituted into the cell0 row, and a second real cell on a
database of its own could not be reached by the processes that already serve the
first.

A second cell would need its own database, its own message bus, its own
conductor and its own console proxy, plus a super-conductor split: the conductor
that talks to the API layer runs apart from the per-cell conductors that talk to
the computes. The shape left open for that is a `spec.cells[]` list beside a
default cell, so the fields a single-cell CR carries today keep describing that
default cell. The follow-ups are tracked in meta issue
[#1014](https://github.com/C5C3/cobaltcore/issues/1014).
