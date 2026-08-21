---
title: dizzy Chaos Testing
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# dizzy Chaos Testing

[dizzy](https://github.com/B42Labs/dizzy) is a scenario-driven load and
consistency tester for OpenStack control planes. Its `chaos` verb runs a
randomized create/mutate/delete churn soak through the OpenStack APIs against a
running ControlPlane. `make dizzy-keystone` and `make dizzy-glance` drive it
against the quick-start stack and export per-operation metrics into the dizzy
VictoriaMetrics for the Grafana dashboards.

For the overlay that installs the VictoriaMetrics + Grafana backend, see
[Infrastructure Manifests](../infrastructure/infrastructure-manifests.md).

## What the chaos verb does

A chaos run loops for a fixed duration, creating, mutating, and deleting
resources at random through the service APIs. It removes the resources it
created when the run ends, including on an interrupt. After cleanup it runs a
leak check to confirm nothing it created outlived the run. Throughout, it
exports per-operation metrics over OTLP at a 15-second interval.

The 5-minute default duration comes from the scenario's `chaos:` block. Override
it with `DIZZY_ARGS="--duration 30m"`.

## Prerequisites

- The dizzy stack installed on the cluster (`WITH_DIZZY=true make deploy-infra`).
  The runners refuse to start when the `dizzy` namespace is absent.
- A Ready quick-start ControlPlane. The runners read the admin password from the
  `controlplane-keystone-admin-credentials` Secret in the `openstack` namespace
  and generate `_output/dizzy/clouds.yaml` (mode 600, cloud key `devstack-c5c3`)
  from it.
- A Go toolchain on PATH. `hack/dizzy.sh` installs `bin/dizzy` with `go install`
  at the pinned version, skipping the install when `go version -m bin/dizzy`
  already reports that version.

The keystone soak's small profile needs admin credentials; it creates one domain
and two roles. The glance soak uploads synthetic images of up to 40 MiB and
needs nothing beyond a member role.

## Running a soak

```bash
# Churn Keystone for the scenario's default 5 minutes.
make dizzy-keystone

# Churn Glance instead.
make dizzy-glance

# Longer run, fixed seed, keep the created resources for inspection.
DIZZY_ARGS="--duration 30m --seed 42 --no-cleanup" make dizzy-keystone
```

Each target runs three separate preflights before handing off to
`hack/dizzy.sh chaos <service>`: kubectl reachability, the `dizzy` namespace, and
the ControlPlane admin Secret. Each failure prints its own message so the three
causes stay distinguishable.

## Variables

Every input is an optional environment override. The dizzy version pin lives only
in `hack/dizzy.sh`.

| Variable | Effect |
| --- | --- |
| `DIZZY_SCENARIO` | Path to an alternate scenario file (default the cached `scenarios/<service>/small.yaml`). |
| `DIZZY_ARGS` | Extra dizzy flags, space-separated (for example `--duration 30m`, `--no-cleanup`, `--seed 42`). Flags with embedded quoted spaces are not supported. |
| `DIZZY_VERSION` | Pin override for the dizzy version. |
| `DIZZY_AUTH_URL` | Keystone auth URL override, skipping the host-port probe. |
| `DIZZY_SECRET` | Name of the ControlPlane admin Secret (default `controlplane-keystone-admin-credentials`). |
| `DIZZY_CP_NAMESPACE` | Namespace of the admin Secret (default `openstack`). |
| `KIND_CLUSTER` | Cluster name for the `docker port` probes (default `cobaltcore`). Note that `make deploy-infra` itself keys off `CLUSTER_NAME`. |

## Watching the run

Grafana serves the dizzy dashboards at `https://dizzy.127-0-0-1.nip.io`; append
`:<KIND_HOST_PORT>` when that override is set to something other than 443. Access
is anonymous Viewer, so no login is needed. Three dashboards ship with the
overlay:

| Dashboard | UID |
| --- | --- |
| dizzy Overview (the anonymous home dashboard) | `dizzy-overview` |
| dizzy API Operations | `dizzy-api-operations` |
| dizzy Time to Ready | `dizzy-time-to-ready` |

## Metric families

Each family carries `cloud`, `scenario`, and `service` labels.

| Metric | Meaning |
| --- | --- |
| `dizzy_operation_duration_seconds` | Duration of a single API operation. |
| `dizzy_resource_time_to_ready_seconds` | Time for a created resource to reach a ready state. |
| `dizzy_iteration_duration_seconds` | Duration of one churn iteration. |
| `dizzy_iteration_operations_total` | Operations performed per iteration. |
| `dizzy_iterations_total` | Iterations completed. |

`dizzy_operation_errors_total` shows up only after an operation has failed, so a
clean run never emits it. List what actually landed after a run:

```bash
curl -fsS http://localhost:8428/api/v1/label/__name__/values
```

## Troubleshooting

The tooling prints two warnings during a run.

**No 30428 port mapping.** When the kind cluster has no host mapping for port
30428, it predates the mapping in `hack/kind-config.yaml` and OTLP ingest cannot
reach VictoriaMetrics. Recreate the cluster to fix it:

```bash
make teardown-infra && WITH_DIZZY=true make deploy-infra
```

**VictoriaMetrics unreachable.** When nothing answers on `localhost:8428`, the
warning notes that metrics are exported into the void. The soak still runs and
exits by dizzy's own result, because dizzy degrades export failures to warnings.
