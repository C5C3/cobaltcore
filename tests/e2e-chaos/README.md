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
  suite **must** be added to the `pod`, `network`, `ovn` or `nova` leg
  there, or it never runs in CI.
- **Runner split:** the `pod` leg is pinned to
  `blacksmith-4vcpu-ubuntu-2404` for now (it is the blocking leg and has not
  been stable on the self-hosted runners); the `network` leg runs on the
  `self-hosted` runners. The NetworkChaos suites
  (`mariadb-network-latency`, `-partition`, `glance-garage-outage`,
  `barbican-openbao-outage`, `neutron-mariadb-outage`,
  `neutron-broker-outage`, `cinder-broker-outage`, `glance-mariadb-outage`,
  `placement-mariadb-outage`, `barbican-mariadb-outage`,
  `cinder-mariadb-outage`, `memcached-network-partition`,
  `nova-broker-outage`, `nova-mariadb-outage`, `nova-placement-outage`) need
  the `sch_netem`/`ip_set` kernel modules, which `hack/deploy-infra.sh` loads
  (installing `linux-modules-extra-$(uname -r)` on demand); the `network`
  leg stays `continue-on-error`. It runs every suite that needs the neutron,
  cinder or ovn operators, the placement-operator for
  `placement-mariadb-outage`, and a diagnostics step that dumps all seven
  operator namespaces; its wall is 135 minutes. The `ovn` leg runs
  `ovn-southbound-outage` on the `self-hosted` runners as well, with
  `WITH_OVN_KERNEL_MODULES=true` so `hack/deploy-infra.sh` loads the
  `openvswitch`/`geneve` modules the chassis DaemonSets need, and only the
  ovn-operator deployed, within a wall of 45 minutes. It stays
  `continue-on-error` too: the suite builds a datapath on the node itself.
  The `nova` leg runs the six suites on the Nova stack (`nova-broker-outage`,
  `nova-mariadb-outage`, `nova-placement-outage`, `nova-keystone-outage`,
  `nova-operator-pod-kill`, `neutron-metadata-agent-pod-kill`) on the
  `self-hosted` runners, with the keystone stack plus the placement, ovn,
  neutron and nova operators, `WITH_MESSAGING=true` (five of the six take a
  vhost on the shared broker, and `nova-broker-outage` brings a
  `RabbitmqCluster` of its own) and `WITH_OVN_KERNEL_MODULES=true` for the
  OVNChassis of `neutron-metadata-agent-pod-kill`, within a wall of 150
  minutes. That leg stays `continue-on-error` as well, because three of its
  suites are NetworkChaos partitions on the same kernel modules. The `pod`
  leg keeps its 90 minutes; the four walls sum to seven hours.
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
- `unseal-openbao.sh` — OpenBao runs single-replica with Shamir sealing
  in kind; after a pod-kill the new pod starts **sealed** and stays 0/1
  forever. This helper re-unseals it (idempotent); production HA Raft
  does not have this problem, kind cannot model that recovery.

Debugging a red chaos job? Use the `debug-e2e-failure` skill
(`.claude/skills/debug-e2e-failure/`).
