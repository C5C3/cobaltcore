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
- **Runners:** all six legs run on the `self-hosted` runners. The
  NetworkChaos suites (`mariadb-network-latency`, `-partition`,
  `glance-garage-outage`, `barbican-openbao-outage`,
  `neutron-mariadb-outage`, `neutron-broker-outage`, `cinder-broker-outage`,
  `glance-mariadb-outage`, `placement-mariadb-outage`,
  `barbican-mariadb-outage`, `cinder-mariadb-outage`,
  `memcached-network-partition`, `nova-broker-outage`,
  `nova-mariadb-outage`, `nova-placement-outage`,
  `korc-network-partition`) need the `sch_netem`/`ip_set` kernel modules,
  which `hack/deploy-infra.sh` loads (installing
  `linux-modules-extra-$(uname -r)` on demand); the `network`
  leg stays `continue-on-error`. It runs every suite that needs the neutron,
  cinder or ovn operators, the placement-operator for
  `placement-mariadb-outage`, and a diagnostics step that dumps all seven
  operator namespaces; its wall is 135 minutes. The `ovn` leg runs
  `ovn-southbound-outage` with `WITH_OVN_KERNEL_MODULES=true` so
  `hack/deploy-infra.sh` loads the `openvswitch`/`geneve` modules the
  chassis DaemonSets need, and only the ovn-operator deployed, within a wall
  of 45 minutes. It stays `continue-on-error` too: the suite builds a
  datapath on the node itself. The `nova` leg runs the six suites on the
  Nova stack (`nova-broker-outage`, `nova-mariadb-outage`,
  `nova-placement-outage`, `nova-keystone-outage`, `nova-operator-pod-kill`,
  `neutron-metadata-agent-pod-kill`) with the keystone stack plus the
  placement, ovn, neutron and nova operators, `WITH_MESSAGING=true` (five of
  the six take a vhost on the shared broker, and `nova-broker-outage` brings
  a `RabbitmqCluster` of its own) and `WITH_OVN_KERNEL_MODULES=true` for the
  OVNChassis of `neutron-metadata-agent-pod-kill`, within a wall of 170
  minutes. That leg stays `continue-on-error` as well, because three of its
  suites are NetworkChaos partitions on the same kernel modules. The
  `controlplane` leg runs the five suites that bring up a ControlPlane of
  their own in a short namespace of their own (`korc-provisioning-outage`,
  `korc-network-partition`, `controlplane-keystone-degraded`,
  `c5c3-operator-pod-kill`, `rabbitmq-operator-cascading-failure`) on the
  `self-hosted` runners, with `WITH_CONTROLPLANE=true` and
  `CONTROLPLANE_OPERATORS=external`, K-ORC and the c5c3-operator deployed
  beside the keystone stack, within a wall of 120 minutes. It stays
  `continue-on-error` too, for the NetworkChaos partition of
  `korc-network-partition`. The `upgrade` leg runs the two suites that fault
  a Cinder release upgrade from 2026.1 to 2026.2
  (`cinder-upgrade-db-partition`, `cinder-upgrade-broker-pod-kill`), the only
  leg that loads a 2026.2 image, with the cinder-operator, `WITH_NFS=true` and
  `WITH_MESSAGING=true`, within a wall of 80 minutes. It stays
  `continue-on-error` as well: its partitions are NetworkPolicies that rest
  on kindnet's enforcement, and its broker kill is new. The `pod` leg runs the
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
