---
name: debug-e2e-failure
description: >-
  Diagnose a failing chainsaw e2e job in the forge CI — resolve the failed
  run, pull the failed-step logs and JUnit/diagnostic evidence, map the
  failure back to the suite directory under tests/, classify it against the
  known flake patterns, and reproduce it locally against a kind cluster.
  Use when a CI e2e job fails (e2e-infra, e2e-operator, e2e-chaos,
  e2e-prometheus, e2e-controlplane, e2e-operator-upgrade, tempest), when
  asked to debug a chainsaw suite, or when reproducing an e2e failure
  locally.
---

# Debug an e2e failure

This skill is a **runbook, not an audit** — it walks a failing e2e CI job
from red check to root cause. The two outcomes it must distinguish: a
**real regression** (fix the code) and a **flake** (fix the test's timing
assumption, as its own commit that names the race).

The e2e stack: kind cluster + FluxCD infra (`hack/deploy-infra.sh`),
operators deployed via `hack/ci-deploy-operator.sh`, tests driven by
[chainsaw](https://kyverno.github.io/chainsaw/) v0.2.14 on Go's testing
framework. Every e2e job dumps `hack/ci-dump-diagnostics.sh` into its job
log and uploads a JUnit report artifact from `_output/reports/`.

## The e2e job families

| Job | What it runs | Where the diagnosis lives |
|---|---|---|
| `e2e-infra` | `tests/e2e/infrastructure/` on bare kind + Flux stack | "Dump diagnostic info" step (on failure) + `e2e-infra-junit-report` |
| `e2e-operator (<op>)` | `tests/e2e/<op>/` plus `tests/e2e/<op>-operator/` if present; `<op>-operator:dev` + one service image per release from `source-refs.yaml` | diag dump (always, `OPERATOR=<op>`) + `e2e-<op>-junit-report` |
| `e2e-operator-upgrade` | `tests/e2e-operator-upgrade/` — last released chart+image from GHCR, then `helm upgrade` to the local build | diag dump + `e2e-operator-upgrade-junit-report` |
| `e2e-chaos` (`pod` / `network`) | explicit `test_dirs` matrix over `tests/e2e-chaos/`, chaos config (`parallel: 1`, `assert: 300s`); `network` leg is `continue-on-error` (kernel modules only on GitHub-hosted runners) | diag dump + `e2e-chaos-junit-report-<suite>` |
| `e2e-prometheus` | `tests/e2e/keystone/prometheus-stack/` with `WITH_PROMETHEUS=true` | diag dump + `e2e-prometheus-junit-report` |
| `e2e-controlplane` | `tests/e2e/c5c3/full-controlplane-keystone/` — full chain (c5c3-operator + K-ORC + keystone-operator), `E2E_REQUIRE_CONTROLPLANE_STACK=true` flips presence-guard SKIPs into failures | diag dump (`OPERATOR=c5c3`) + `e2e-controlplane-junit-report` |
| `tempest (<release>)` | `hack/ci-run-tempest.sh` against a Ready Keystone CR from `tests/tempest/<svc>-<slug>/` | `tempest-<release>-results` artifact (JUnit + logs; `tempest.conf` excluded — carries the admin password) |

Two gating facts worth knowing before reading results: the happy-path
config (`tests/e2e/chainsaw-config.yaml`) runs `parallel: 4` with
`failFast: true`, so everything after the first failure may be cascade or
never ran; and chainsaw v0.2.14's `--include/--exclude-test-regex` flags
are **no-ops** (config field set but never read), which is why the chaos
job enumerates `test_dirs` explicitly in `ci.yaml`.

## Procedure

### 1. Collect the evidence

```bash
bash .claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh --pr <number>
# or, when you already know the run:
bash .claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh --run <run-id>
```

It resolves the newest failed run, lists the failed jobs, downloads the
failed-step logs to `_output/e2e-failure/run-<id>/failed-jobs.log`,
extracts a condensed excerpt (`chainsaw-excerpt.log`), maps failed
chainsaw test names back to their `chainsaw-test.yaml`, and lists the
downloadable artifacts. Exit 2 means the run is still in progress —
GitHub serves no logs until it completes.

### 2. Read the chainsaw failure block

Work top-down in the excerpt:

- `--- FAIL: chainsaw/<test-name>` is the authoritative marker; the
  test name equals `metadata.name` in the suite's `chainsaw-test.yaml`.
  With `failFast: true`, diagnose the **first** FAIL only.
- The step table (`| HH:MM:SS | <test> | <step> | <OP> | ERROR |`) tells
  you which step and operation died: `ASSERT` timeouts print an
  expected-vs-actual diff; `SCRIPT`/`EXEC` steps print their stdout.
- `catch:` blocks in the suite (and `tests/e2e-chaos/diagnostics.sh`)
  dump CR conditions, pod logs, and events right below the failure —
  that output is usually where the actual cause sits, e.g. a failed
  startup probe or an `ExternalSecret` `UpdateFailed` event.

### 3. Read the diagnostic dump

The "Dump diagnostic info" step (`hack/ci-dump-diagnostics.sh`) prints,
in order: HelmReleases, all pods, events (last 50), Flux state, and —
when `OPERATOR` is set — operator logs, Job describes+logs, every pod
log in the `openstack` namespace, and the CR's `conditions:` block.
Match its timestamps against the chainsaw step table: the question is
always "what was the cluster doing when the assert timed out".

### 4. Classify: regression or known pattern?

Check the failure against the table below before writing a fix. If the
failure is a CRD schema mismatch ("unknown field", "invalid value"),
switch to [[check-fixture-drift]]; if webhook and CRD disagree, to
[[check-crd-drift]].

### 5. Reproduce locally

```bash
make deploy-infra                      # kind + Flux stack; add opt-ins:
# WITH_CHAOS_MESH=true    -> e2e-chaos suites
# WITH_PROMETHEUS=true    -> prometheus-stack suite
# WITH_CONTROLPLANE=true  -> full-controlplane-keystone chain
OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator hack/ci-deploy-operator.sh
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/<suite>/
```

Family-specific constraints:

- `make e2e-chaos` / `e2e-prometheus` / `e2e-controlplane` carry
  preflights that name the missing opt-in — trust their remediation hint.
- `make e2e-operator-upgrade` must run against a cluster **without** a
  pre-deployed keystone-operator (the suite installs the released
  baseline itself).
- After an OpenBao pod kill, run `tests/e2e-chaos/unseal-openbao.sh` —
  the single-replica kind topology has no auto-unseal.

### 6. Fix with the right shape

- A status flip observed before a dependent object exists → replace the
  single-shot `kubectl get` with a chainsaw `assert` (it polls to the
  step timeout). See `5b0c961a`.
- A genuinely transient boundary (webhook just rolled, registry
  eventually consistent) → a bounded retry loop **plus** an explicit
  step timeout larger than one failed attempt. See `85e3172a`.
- A hard-coded timing window coupled to spec defaults → derive the
  window from the live rendered object and fail loudly when the
  extraction comes back empty. See `0fa60e6e`.
- Never widen a timeout without a comment stating the budget it absorbs
  (`tests/e2e/chainsaw-config.yaml` `cleanup: 3m` is the template).

## Known failure patterns

| Symptom | Likely cause | Reference fix |
|---|---|---|
| `context deadline exceeded` / `InternalError` on the first webhook-gated kubectl right after an operator rollout | apiserver reuses a stale keep-alive to a terminated webhook pod IP | `85e3172a` — retry loop with fresh dials + 120s step timeout |
| Single-shot `kubectl get <job/pod>` fails although the object appears seconds later | script raced the controller's next reconcile pass (status flips before the object is created) | `5b0c961a` — chainsaw `assert` instead of one-shot get |
| Helper script targets the wrong namespace, `pods "openbao-0" not found` | chainsaw injects `NAMESPACE=<test ns>` into every script step, poisoning generic env vars | `2c23003e` — dedicated `OPENBAO_NAMESPACE` contract |
| Availability-sampling loop flakes when a CR raises `terminationGracePeriodSeconds`/preStop | hard-coded window silently coupled to spec defaults | `0fa60e6e` — derive window from the rendered Deployment |
| Cleanup timeout exceeded in deletion suites under `parallel: 4` | MariaDB operator serializes Database/User/Grant deletions past the old 60s window | `cleanup: 3m` in `tests/e2e/chainsaw-config.yaml` |
| Image pull / manifest inspect flakes against ghcr.io | registry eventual consistency, transient 5xx | `4b3cfbde`, `1f43aee7`, `f67318cd` — bounded retries; `b5efce28` — GHCR transport replaced artifact download |
| OpenBao pod 0/1 Running forever after a chaos kill | single-replica Shamir sealing — new pod starts sealed, no auto-unseal in kind | `tests/e2e-chaos/unseal-openbao.sh` |
| NetworkChaos suites fail only on Blacksmith runners | Firecracker microVM kernel lacks `ip_set`/`xt_set`/`sch_netem` | chaos matrix `network` leg pinned to `ubuntu-24.04`, `continue-on-error` |
| Individual tempest test fails intermittently in one release row | upstream test races under concurrency | `ca194368` — failed tests rerun once serially, rewritten as flakes in JUnit |

## Notes

- This skill is read-only; the collect script only talks to GitHub via
  `gh` and writes under `_output/e2e-failure/`. Fixes are a separate,
  explicitly-scoped task.
- `_output/` is untracked but (at HEAD) not gitignored — don't commit
  collected evidence.
- Flake fixes land as their own commit whose message names the race and
  why the fix bounds it (see `85e3172a` for the calibration).
- Suites live directory-per-suite and are auto-discovered — see
  `tests/e2e/README.md` and `tests/e2e-chaos/README.md` for the
  conventions; new **chaos** suites must additionally be added to the
  `test_dirs` matrix in `ci.yaml` (the regex flags don't filter).
- Pair with [[check-fixture-drift]] for schema mismatches and
  [[check-crd-drift]] when the webhook rejects what the CRD allows.
