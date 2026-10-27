<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Chaos e2e suites

Chaos Mesh fault-injection tests: kill or degrade a dependency (MariaDB,
Memcached, OpenBao, the operator itself) and assert the operator's
recovery contract. Requires the opt-in stack:

```bash
WITH_CHAOS_MESH=true make deploy-infra
make e2e-chaos            # preflights fail fast with a remediation hint
```

## Conventions (deltas from tests/e2e/)

The base conventions — directory per suite, numbered fixtures,
`chainsaw-test.yaml`, auto-discovery by `make e2e-chaos` — are the same
as in `tests/e2e/README.md`. Chaos-specific rules:

- **Own config:** `tests/e2e-chaos/chainsaw-config.yaml` runs
  `parallel: 1` (suites mutate shared infrastructure pods — serial
  execution prevents cross-test interference), `assert: 300s` (recovery
  spans several reconcile cycles plus pod restarts), `cleanup: 120s`
  (Chaos Mesh CRs take time to release injected faults).
- **CI is NOT auto-discovered.** The `e2e-chaos` job in
  `.github/workflows/ci.yaml` enumerates `test_dirs` explicitly per
  matrix leg (chainsaw's include/exclude-regex flags match the test name
  `chainsaw/<metadata.name>`, not a directory, so they are not used). A new
  suite **must** be added to the `pod`, `network`, `ovn`, `nova`,
  `controlplane` or `upgrade` leg there, or it never runs in CI.
- **Runners and gating:** all six legs run on the `self-hosted` runners,
  every leg gates, and every leg runs against `main` each night at 22:00
  UTC with `--fail-fast=false`; `report-nightly-chaos` files a red night
  as one issue per failed suite. The
  NetworkChaos suites (`mariadb-network-latency`, `-partition`,
  `glance-garage-outage`, `barbican-openbao-outage`,
  `neutron-mariadb-outage`, `neutron-broker-outage`, `cinder-broker-outage`,
  `glance-mariadb-outage`, `placement-mariadb-outage`,
  `barbican-mariadb-outage`, `cinder-mariadb-outage`,
  `memcached-network-partition`, `nova-broker-outage`,
  `nova-mariadb-outage`, `nova-placement-outage`,
  `korc-network-partition`) need the `sch_netem`/`ip_set` kernel modules,
  which `hack/deploy-infra.sh` loads (installing
  `linux-modules-extra-$(uname -r)` on demand). The `network`
  leg runs every suite that needs the neutron,
  cinder or ovn operators, the placement-operator for
  `placement-mariadb-outage`, and a diagnostics step that dumps all seven
  operator namespaces; its wall is 135 minutes. The `ovn` leg runs
  `ovn-southbound-outage` with `WITH_OVN_KERNEL_MODULES=true` so
  `hack/deploy-infra.sh` loads the `openvswitch`/`geneve` modules the
  chassis DaemonSets need, and only the ovn-operator deployed, within a wall
  of 45 minutes. The `nova` leg runs the seven suites on the
  Nova stack (`nova-broker-outage`, `nova-mariadb-outage`,
  `nova-placement-outage`, `nova-keystone-outage`, `nova-operator-pod-kill`,
  `neutron-metadata-agent-pod-kill`, `novacompute-drain-compute-kill`, the
  last after the metadata agent suite's chassis is gone) with the keystone
  stack plus the placement, ovn, neutron and nova operators and the
  `nova-compute` image, `WITH_MESSAGING=true` (six of the seven take a vhost
  on the shared broker, and `nova-broker-outage` brings a `RabbitmqCluster`
  of its own) and `WITH_OVN_KERNEL_MODULES=true` for the OVNChassis of
  `neutron-metadata-agent-pod-kill` and `novacompute-drain-compute-kill`,
  within a wall of 170 minutes. The
  `controlplane` leg runs the eight suites that bring up a ControlPlane of
  their own in a short namespace of their own (`korc-provisioning-outage`,
  `korc-network-partition`, `controlplane-keystone-degraded`,
  `c5c3-operator-pod-kill`, `rabbitmq-operator-cascading-failure`,
  `controlplane-provisioning-mariadb-kill`,
  `controlplane-deletion-korc-outage`,
  `keystoneuser-rotation-keystone-outage`) on the
  `self-hosted` runners, with `WITH_CONTROLPLANE=true` and
  `CONTROLPLANE_OPERATORS=external`, K-ORC and the c5c3-operator deployed
  beside the keystone stack, within a wall of 120 minutes.
  `controlplane-provisioning-mariadb-kill` sits under the entry's
  `isolated_test_dirs` and runs after the others in an invocation of its
  own: it fails on the current keystone-operator, and `failFast` would skip
  the suites behind it. The leg gates like every other, so the suite keeps
  it red until the keystone-operator recovers an interrupted first
  `db_sync`. The `upgrade` leg runs the two suites that fault
  a Cinder release upgrade from 2026.1 to 2026.2
  (`cinder-upgrade-db-partition`, `cinder-upgrade-broker-pod-kill`), the only
  leg that loads a 2026.2 image, with the cinder-operator, `WITH_NFS=true` and
  `WITH_MESSAGING=true`, within a wall of 80 minutes. Its partitions are
  NetworkPolicies that rest on kindnet's enforcement. The `pod` leg runs the
  eight platform suites of #1390 that need only the standalone stack and
  `fernet-rotation-operator-outage` on top of its own, within 135 minutes; the six walls sum to 685 minutes, which the
  author of #1394 accepted on 2026-10-25.
- **Shared state is restored in every catch.** A step that changes shared
  platform state (a scale-down, a narrowed policy, a sealed OpenBao, a chaos
  object) undoes it in its own `catch:`, and so does every later step up to
  the one that lifts the fault. The legs run `parallel: 1`, so a leftover
  fails every later suite of the leg.
- **Flux has no suite.** A killed Flux is on no request path, and a restarted
  helm-controller can upgrade a platform chart in the middle of a run; the
  reference page gives the full reason under Components without a fault suite.
- **Recovery assertions are UID-gated** where the contract is "a new pod
  recovered" (compare the pod UID before/after the kill), and
  restart-count-gated where the contract is "survived without restart".
- **Workload budget:** a suite that calls `lib/workload.sh start` carries one
  `# Chaos budget: failures=<N> recovery=<S>s` line in its header comment,
  with the three kind runs it was measured from in a
  `# Chaos budget measured:` line under it. A measured line whose environment
  reads `estimate` holds an estimate no run has measured yet. `verify` refuses a header with
  none, two, or a malformed one. A suite without a workload says why in a
  `# Chaos workload: none (<reason>)` line.
- **Matrix and scenario lines:** every suite header carries one or more
  `# Chaos matrix: <component> <role> <kind>` lines, above the budget line
  or last in the header, and a suite with an ID of the architecture's chaos
  catalog a `# Chaos scenario: SC-CHAOS-<NNN>` line. Run
  `make gen-chaos-matrix` after changing one: `hack/gen-chaos-matrix.sh`
  generates the component x role matrix and the suite table of the chaos
  page from them, and `make verify-chaos-matrix` fails on a stale page.

## Shared helpers

- `diagnostics.sh baseline|chaos <cr-name> [options]` — call from
  `catch:` blocks for uniform failure dumps (CR conditions, dependency
  pods, Chaos Mesh status, logs, events).
- `lib/workload.sh start|mark|verify|dump|stop <id>` — sends OpenStack
  traffic through the fault from a pod running `lib/workload.py` on
  `ghcr.io/c5c3/tempest:2026.1`, then judges the records and the state of
  OpenStack against the budget line. A local run pulls the Tempest image from
  GHCR unless `kind load docker-image` put it on the node. See
  `docs/reference/testing/chaos-e2e-tests.md` (Workload and Consistency
  Harness).
- `lib/controlplane.sh` — the ControlPlane bring-up of the `controlplane`
  leg, sourced by its suites' scripts: seed the per-CR OpenBao paths, apply,
  wait for each stage within budgets that fit the 40-minute bring-up step,
  put the suite's namespace into `@NS@` fixtures, tear the plane down within
  12 minutes, and dump it for a `catch:` block.
- `lib/gateway.sh`, `lib/openbao.sh`, `lib/pods.sh` — sourced helpers: the
  ClusterIP of the proxy of Gateway `openstack-gw`; the root token and `bao`
  calls into `openbao-0`, with the token on stdin; and the UID-gated wait for
  the pods of a Deployment a suite killed.
- `unseal-openbao.sh` — OpenBao runs single-replica with Shamir sealing
  in kind; after a pod-kill the new pod starts **sealed** and stays 0/1
  forever. This helper re-unseals it (idempotent); production HA Raft
  does not have this problem, kind cannot model that recovery.

Debugging a red chaos job? Use the `debug-e2e-failure` skill
(`.claude/skills/debug-e2e-failure/`).
