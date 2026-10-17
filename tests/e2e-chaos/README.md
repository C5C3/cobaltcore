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

The base conventions are the same as in `tests/e2e/README.md`: a directory
per suite, numbered fixtures, `chainsaw-test.yaml`, and auto-discovery by
`make e2e-chaos`. Chaos-specific rules:

- **Own config:** `tests/e2e-chaos/chainsaw-config.yaml` runs
  `parallel: 1` (suites mutate shared infrastructure pods, and serial
  execution prevents cross-test interference), `assert: 300s` (recovery
  spans several reconcile cycles plus pod restarts), `cleanup: 120s`
  (Chaos Mesh CRs take time to release injected faults).
- **CI is NOT auto-discovered.** The `e2e-chaos` job in
  `.github/workflows/ci.yaml` enumerates `test_dirs` explicitly per
  matrix entry (chainsaw's include/exclude-regex flags match the test name
  `chainsaw/<metadata.name>`, not a directory, so they are not used). A new
  suite **must** be added to the `pod`, `network`, `ovn`, `nova`,
  `controlplane` or `faults` entry there, or it never runs in CI. The one
  exception is `nova-instances-io-fault`, which needs the libvirt stack of the
  `e2e-nova-libvirt` job and runs in its step `Run the libvirt chaos suite`.
- **Runners:** all six entries run on the `self-hosted` runners, and all but
  `pod` are `continue-on-error`. The NetworkChaos suites
  (`mariadb-network-latency`, `-partition`, `glance-garage-outage`,
  `barbican-openbao-outage`, `neutron-mariadb-outage`,
  `neutron-broker-outage`, `cinder-broker-outage`, `glance-mariadb-outage`,
  `placement-mariadb-outage`, `barbican-mariadb-outage`,
  `cinder-mariadb-outage`, `memcached-network-partition`,
  `nova-broker-outage`, `nova-mariadb-outage`, `nova-placement-outage`,
  `korc-network-partition`) need the `sch_netem`/`ip_set` kernel modules,
  which `hack/deploy-infra.sh` loads (installing
  `linux-modules-extra-$(uname -r)` on demand). HTTPChaos needs `bridge`,
  `ebtables`, `ebtable_broute`, `iptable_mangle`, `xt_TPROXY` and `xt_socket`,
  and IOChaos needs `fuse`, which the same script loads best effort.
  - `network` runs every suite that needs the neutron, cinder or ovn
    operators, among them the ten Cinder suites, the placement-operator for
    `placement-mariadb-outage`, and a diagnostics step that dumps all seven
    operator namespaces, within 175 minutes.
  - `ovn` runs `ovn-southbound-outage` and `ovn-raft-clock-skew` with
    `WITH_OVN_KERNEL_MODULES=true`, so `hack/deploy-infra.sh` loads the
    `openvswitch`/`geneve` modules the chassis DaemonSets need, and only the
    ovn-operator deployed, within 45 minutes.
  - `nova` runs the eight suites on the Nova stack with the keystone stack
    plus the placement, ovn, neutron and nova operators, `WITH_MESSAGING=true`
    and `WITH_OVN_KERNEL_MODULES=true` for the OVNChassis of
    `neutron-metadata-agent-pod-kill` and `nova-compute-neutron-http-delay`,
    within 190 minutes.
  - `controlplane` runs the five suites that bring up a ControlPlane of their
    own in a short namespace of their own, with `WITH_CONTROLPLANE=true`,
    `CONTROLPLANE_OPERATORS=external`, K-ORC and the c5c3-operator beside the
    keystone stack, within 100 minutes.
  - `pod`, the one blocking entry, runs the PodChaos suites of the keystone
    stack and the eight platform suites of #1390 that need only the
    standalone stack, within 120 minutes.
  - `faults` runs the nine suites of #1391 that need only the standalone
    stack, with the placement-operator, `WITH_METRICS_SERVER=true` and
    `WITH_VPA=true`, within 130 minutes.

  The six walls sum to 760 minutes. `e2e-nova-libvirt` runs its two suites
  within 120 minutes.
- **Shared state is restored in every catch.** A step that changes shared
  platform state (a scale-down, a narrowed policy, a sealed OpenBao, a chaos
  object) undoes it in its own `catch:`, and so does every later step up to
  the one that lifts the fault. Each entry runs `parallel: 1`, so a leftover
  fails every later suite of the entry.
- **HTTPChaos, TimeChaos, IOChaos, StressChaos and DNSChaos** follow the
  shape of `lib/faults.sh`:
  - every chaos object carries the label `chaos.c5c3.io/suite: <suite>` and
    lives in the suite's namespace, and every step from the first injection
    to the lift deletes the suite's objects by that label in its `catch:`;
  - a phase applies its object, waits for `AllInjected`, runs a canary that
    prints `INJECTED-<PHASE>-OK`, asserts, then deletes the object and waits
    for the delete; a canary that sees no effect prints
    `FAIL: <kind>/<name> reports AllInjected but <observation>`;
  - several phases share one fault window: `mark fault-start` runs right
    before the first apply, `mark fault-end` right after the last delete;
  - an object that needs a run-time value carries `@NAME@` placeholders
    that `fault_render` fills.
- **HTTPChaos sits server-side on API pods.** The four HTTPChaos suites
  (`nova-placement-http-fault`, `nova-compute-neutron-http-delay`,
  `cinder-glance-http-abort`, `keystone-validation-http-503`) select the API
  pods of a service by `app.kubernetes.io/name`, filter by method and path,
  and restart the client once the fault is injected: HTTPChaos reaches only
  connections opened after it. The ValidatingAdmissionPolicy
  `chaos-mesh-httpchaos-api-workloads-only` (`deploy/kind/chaos-mesh/`)
  denies an HTTPChaos on any other workload, because chaos-tproxy rewires the
  network namespace of the pod it selects, which for a `hostNetwork` pod is
  the node's.
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

- `diagnostics.sh baseline|chaos <cr-name> [options]` is called from
  `catch:` blocks for uniform failure dumps (CR conditions, dependency
  pods, the chaos objects of all seven kinds, logs, events).
- `lib/workload.sh start|mark|verify|dump|stop <id>` sends OpenStack
  traffic through the fault from a pod running `lib/workload.py` on
  `ghcr.io/c5c3/tempest:2026.1`, then judges the records and the state of
  OpenStack against the budget line. A local run pulls the Tempest image from
  GHCR unless `kind load docker-image` put it on the node. See
  `docs/reference/testing/chaos-e2e-tests.md` (Workload and Consistency
  Harness).
- `lib/controlplane.sh` is the ControlPlane bring-up of the `controlplane`
  entry, sourced by its suites' scripts: seed the per-CR OpenBao paths, apply,
  wait for each stage within budgets that fit the 40-minute bring-up step,
  put the suite's namespace into `@NS@` fixtures, tear the plane down within
  12 minutes, and dump it for a `catch:` block.
- `lib/gateway.sh`, `lib/openbao.sh` and `lib/pods.sh` are sourced helpers:
  the ClusterIP of the proxy of Gateway `openstack-gw`; the root token and
  `bao` calls into `openbao-0`, with the token on stdin; and the UID-gated
  wait for the pods of a Deployment a suite killed.
- `lib/faults.sh` is the fault phase of the HTTPChaos, TimeChaos, IOChaos,
  StressChaos and DNSChaos suites, sourced by their scripts. It renders an
  object's `@NAME@` placeholders, applies it and waits for `AllInjected`,
  lifts it and waits for the delete, runs the `dd`, resolver and cgroup
  canaries, and starts and reads probe pods and Jobs. `lib/probe.py` is the
  program those probe pods run: HTTP checks with or without a fresh token,
  and the Cinder volume and backup waits.
- `unseal-openbao.sh` re-unseals OpenBao, which runs single-replica with
  Shamir sealing in kind: after a pod-kill the new pod starts **sealed** and
  stays 0/1 forever. The helper is idempotent. Production HA Raft does not
  have this problem, and kind cannot model that recovery.

Debugging a red chaos job? Use the `debug-e2e-failure` skill
(`.claude/skills/debug-e2e-failure/`).
