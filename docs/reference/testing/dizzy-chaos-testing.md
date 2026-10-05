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
[Infrastructure Manifests](../infrastructure/infrastructure-manifests.md#dizzy-load-chaos-stack-kind-only-opt-in)
for kind and [Lab dizzy stack](../infrastructure/infrastructure-manifests.md#lab-dizzy-stack)
for the metal-stack lab.

## What the chaos verb does

A chaos run loops for a fixed duration, creating, mutating, and deleting
resources at random through the service APIs. It removes the resources it
created when the run ends, including on an interrupt. After cleanup it runs a
leak check to confirm nothing it created outlived the run. Throughout, it
exports per-operation metrics over OTLP at a 15-second interval.

The 5-minute default duration comes from the scenario's `chaos:` block. Override
it with `DIZZY_ARGS="--duration 30m"`.

## Prerequisites

- The dizzy stack installed on the cluster (`WITH_DIZZY=true make deploy-infra`;
  on the metal-stack lab `EXTERNAL_CLUSTER=true WITH_DIZZY=true` beside the
  lab's other flags). The runners refuse to start when the `dizzy` namespace
  is absent.
- On the metal-stack lab, two port-forwards from the workstation: the
  Gateway's on local port 8443 and one to VictoriaMetrics on local port 8428
  (see [Running a soak](#running-a-soak)).
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

On the metal-stack lab nothing outside the cluster reaches the stack. Open the
two port-forwards, each in a terminal of its own, then run the soak with
`EXTERNAL_CLUSTER=true`:

```bash
kubectl -n envoy-gateway-system port-forward \
  "$(kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw -o name)" 8443:443
kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428
EXTERNAL_CLUSTER=true make dizzy-keystone
```

`hack/dizzy.sh` then takes the Keystone URL
`https://keystone.127-0-0-1.nip.io:8443/v3` of the first port-forward, exports
to `localhost:8428` through the second, and calls no `docker`. Neither
script opens a port-forward; restart one that has ended.

The admin password goes only to a Keystone that holds one of the Gateway's
keys. `hack/dizzy.sh` collects the Gateway's certificates from the Secrets
`openstack/*-nip-io-tls` with `jq`, as
[Step 7](../../quick-start-metal-stack.md#cp-verify) of the Quick Start
(metal-stack) does, into `_output/dizzy/gateway-ca.pem`, and names that file as
`cacert` in `clouds.yaml`. dizzy then verifies Keystone and the endpoints of
its catalog against them. Without such a certificate the script exits 1 and
writes no `clouds.yaml`. The kind mode writes `verify: false`.

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
| `KIND_CLUSTER` | Cluster name for the `docker port` probes (default `cobaltcore`). Note that `make deploy-infra` itself keys off `CLUSTER_NAME`. Not read under `EXTERNAL_CLUSTER=true`. |
| `EXTERNAL_CLUSTER` | `true` runs against the metal-stack lab: the Keystone URL of the Gateway port-forward on local port 8443, verified against the Gateway's certificates, no `docker port` probe, and a hint to open the VictoriaMetrics port-forward when `localhost:8428` does not answer. Any other value keeps the kind mode. `make deploy-infra` and `make teardown-infra` read the same variable. |

## Watching the run

Grafana serves the dizzy dashboards at `https://dizzy.127-0-0-1.nip.io`; append
`:<KIND_HOST_PORT>` when that override is set to something other than 443. On
the metal-stack lab it answers at `https://dizzy.127-0-0-1.nip.io:8443` through
the Gateway port-forward. Access is anonymous Viewer, so no login is needed.
Three dashboards ship with the overlay:

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
reach VictoriaMetrics. Only the kind mode probes the mapping. Recreate the
cluster to fix it:

```bash
make teardown-infra && WITH_DIZZY=true make deploy-infra
```

**VictoriaMetrics unreachable.** When nothing answers on `localhost:8428`, the
warning notes that metrics are exported into the void. The soak still runs and
exits by dizzy's own result, because dizzy degrades export failures to warnings.
Under `EXTERNAL_CLUSTER=true` a third line names the port-forward that is
missing:

```text
         Open the port-forward first: kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428
```
