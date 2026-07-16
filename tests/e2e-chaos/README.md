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
  matrix leg (chainsaw v0.2.14's include/exclude-regex flags are
  no-ops). A new suite **must** be added to the `pod` or `network` leg
  there, or it never runs in CI.
- **Runner split:** PodChaos suites (e.g. `glance-operator-pod-kill`) run
  on Blacksmith runners; the NetworkChaos suites
  (`mariadb-network-latency`, `-partition`, `glance-garage-outage`) need
  the `sch_netem`/`ip_set` kernel modules that only GitHub-hosted
  `ubuntu-24.04` runners provide, and that leg is `continue-on-error`.
- **Recovery assertions are UID-gated** where the contract is "a new pod
  recovered" (compare the pod UID before/after the kill), and
  restart-count-gated where the contract is "survived without restart".

## Shared helpers

- `diagnostics.sh baseline|chaos <cr-name> [options]` — call from
  `catch:` blocks for uniform failure dumps (CR conditions, dependency
  pods, Chaos Mesh status, logs, events).
- `unseal-openbao.sh` — OpenBao runs single-replica with Shamir sealing
  in kind; after a pod-kill the new pod starts **sealed** and stays 0/1
  forever. This helper re-unseals it (idempotent); production HA Raft
  does not have this problem, kind cannot model that recovery.

Debugging a red chaos job? Use the `debug-e2e-failure` skill
(`.claude/skills/debug-e2e-failure/`).
