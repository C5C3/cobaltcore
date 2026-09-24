---
name: debug-e2e-failure
description: >-
  Diagnose a failing e2e or Tempest job in the CobaltCore CI — resolve the
  failed run, pull the job logs, annotations and JUnit/diagnostic evidence,
  scan them for the known failure signatures, decide whether the fault is
  infrastructure or the change, map the failure back to the suite directory
  under tests/, and reproduce it locally against a kind cluster. Use when a CI
  e2e job fails (build-e2e-images, e2e-infra, e2e-operator, e2e-chaos,
  e2e-prometheus, e2e-controlplane, e2e-controlplane-sso,
  e2e-external-keystone, e2e-operator-upgrade, e2e-multicluster,
  e2e-ovn-overlay, tempest), when a job died without logs, when asked to
  debug a chainsaw suite, or when reproducing an e2e failure locally.
---

# Debug an e2e failure

This skill is a **runbook, not an audit** — it walks a failing e2e CI job
from red check to root cause. It must separate three outcomes: an
**infrastructure fault** (runner, registry, runner kernel, a pin that
expired — nothing in the diff), a **real regression** (fix the code), and a
**flake** (fix the test's timing assumption, in its own commit that names the
race). The first step of triage is deciding which of these you are in.

The e2e stack: kind cluster + FluxCD infra (`hack/deploy-infra.sh`, wrapped by
`.github/actions/setup-e2e-infra`), images built once by `build-e2e-images`
and pulled from GHCR by `.github/actions/load-e2e-images`, operators deployed
by `hack/ci-deploy-operator.sh`, tests driven by
[chainsaw](https://kyverno.github.io/chainsaw/) at the version
`CHAINSAW_VERSION` pins in `hack/install-test-deps.sh`, on Go's testing
framework. Every e2e job runs `hack/ci-dump-diagnostics.sh` with
`if: always()`, uploads a JUnit report from `_output/reports/`, and deletes its
kind cluster last (`hack/ci-delete-kind-cluster.sh`).

## The e2e job families

All jobs below run on `pull_request` only (green `main` pushes say nothing
about them); every one except `e2e-infra` gates on `build-e2e-images`, which
skips for fork PRs. Runner `self-hosted` is the `ogrm-*` pool. Re-derive this table
from `.github/workflows/ci.yaml` when it looks stale.

| Job | Runner, wall | What it runs | Where the diagnosis lives |
|---|---|---|---|
| `build-e2e-images` | self-hosted, 120m | `hack/ci-resolve-e2e-images.sh` builds only images whose sources changed and resolves every other to the digest main last published; pushes `e2e-<run_id>-<tag>`. `needs:` lint, shellcheck, test, test-integration, verify-codegen, verify-invalid-cr-fixtures, chainsaw-lint — one lost upstream leg skips the whole e2e chain | its own log: the `Image map` group lists each image as built or `(reused)` |
| `e2e-infra` | self-hosted, 50m | `tests/e2e/infrastructure/` three times: fresh deploy; after a same-parameter `make deploy-infra` re-run (`chainsaw-report-rerun`); a scoped set after an additive `WITH_METRICS_SERVER=true WITH_NFS=true` re-run (`chainsaw-report-additive`) | dump without `OPERATOR` + `e2e-infra-junit-report` |
| `e2e-operator (<op>)` | keystone leg `blacksmith-4vcpu-ubuntu-2404`, others self-hosted; 150m nova, 68m others | matrix over the changed operators of `ALL_OPERATORS` in ci.yaml (keystone c5c3 horizon glance placement barbican ovn neutron cinder nova): `tests/e2e/<op>/` plus `tests/e2e/<op>-operator/`; `--parallel 2` on neutron, cinder, nova. Neutron also deploys the ovn-operator; nova deploys keystone, placement, glance, ovn, neutron; c5c3 installs the sibling CRDs and K-ORC (`hack/ci-deploy-korc.sh`) but no sibling controller. Opt-ins: `WITH_OVN_KERNEL_MODULES` ovn/neutron, `WITH_NFS` cinder, `WITH_MESSAGING` cinder/nova | dump `OPERATOR=<op>` (+ `OPERATOR=ovn` on neutron, + one `OPERATOR_ONLY=1` pass per sibling on nova) + `e2e-<op>-junit-report` |
| `e2e-operator-upgrade` | self-hosted, 68m | `tests/e2e-operator-upgrade/` (own config): last released keystone chart + `:latest` image (`hack/ci-fetch-released-operator.sh`), then `helm upgrade` to the local build | dump `OPERATOR=keystone` + `e2e-operator-upgrade-junit-report` |
| `e2e-chaos (<suite>)` | `pod` leg blacksmith-4vcpu and blocking; `network`, `ovn`, `nova` legs self-hosted and `continue-on-error`; 150m nova, 90m others | the explicit `test_dirs` of each matrix entry in ci.yaml under `tests/e2e-chaos/chainsaw-config.yaml` (`parallel: 1`, `assert: 300s`); runs only after every `e2e-operator` leg finished without failure | dump `OPERATOR=keystone` (`ovn` on the ovn leg; nova leg adds one `OPERATOR_ONLY=1` pass per operator) + `e2e-chaos-junit-report-<suite>` |
| `e2e-prometheus` | self-hosted, 68m | `tests/e2e/keystone/prometheus-stack/` with `WITH_PROMETHEUS=true` | dump `OPERATOR=keystone` + `e2e-prometheus-junit-report` |
| `e2e-controlplane` | self-hosted, 240m | the full ControlPlane with eight services (keystone, horizon, glance, placement, barbican, neutron with a standalone OVNCentral + OVNChassis + metadata agent, cinder on the kind NFS export and shared broker, nova with a fake compute), ten operators + K-ORC. Three sequential chainsaw steps, each its own invocation so failFast cannot cascade: `tests/e2e/c5c3/full-controlplane-keystone/`, then `keystone-service-foreign-namespace/` (`chainsaw-report-keystone-service`), then `keystone-service/` (`chainsaw-report-keystone-service-own-namespace`); `E2E_REQUIRE_CONTROLPLANE_STACK=true` turns presence-guard SKIPs into failures | dumps `OPERATOR=c5c3`, `cinder`, `nova` (all read the `openstack` namespace; the registration suites print their own evidence in `catch`) + `e2e-controlplane-junit-report` |
| `e2e-controlplane-sso` | self-hosted, 90m | `tests/e2e-controlplane-sso/` (federated ControlPlane `controlplane-sso`: websso, Keycloak, OpenLDAP) on keystone + horizon + c5c3 + K-ORC, glance/placement CRDs only | dump `OPERATOR=c5c3` + `e2e-controlplane-sso-junit-report` |
| `e2e-external-keystone` | self-hosted, 90m | `tests/e2e/c5c3/external-keystone/`: External-mode ControlPlanes against a plain SQLite Keystone the operators do not own | dump `OPERATOR=c5c3` + `e2e-external-keystone-junit-report` |
| `e2e-multicluster` | self-hosted, 90m | `make e2e-multicluster` over `tests/e2e-multicluster/`: `cobaltcore-target` runs the infrastructure (`INFRA_ONLY=true` + the `deploy/target-cluster/target-cluster-access` chart), `cobaltcore-mgmt` runs keystone, barbican, ovn and neutron operators, which reach the target only through the chart's token in Secret `cobaltcore-target` in `c5c3-clusters` | two dumps (management `OPERATOR=keystone`; target without `OPERATOR`) + `e2e-multicluster-junit-report` |
| `e2e-ovn-overlay` | self-hosted, 60m, `continue-on-error` | `make e2e-ovn-overlay` over `tests/e2e-ovn-overlay/` (own config) on `hack/kind-config-multinode.yaml` (one control plane, two workers), ovn-operator only; needs openvswitch + geneve on the host | dump `OPERATOR=ovn` + `e2e-ovn-overlay-junit-report` |
| `tempest (<service>, <release>, …)` | self-hosted; 150m nova, 120m cinder, 68m others | matrix from `hack/ci-generate-tempest-matrix.sh`: keystone glance barbican neutron cinder nova × every `releases/*/`; the job's own steps apply the CRs from `tests/tempest/<svc>-<slug>/` and `kubectl wait` each, then `hack/ci-run-tempest.sh`. Runs only after e2e-infra, e2e-operator, e2e-chaos and e2e-prometheus finished without failure. The job name carries every matrix value | `tempest-<service>-<release>-results` (`tempest-results.xml` has the tracebacks the job log omits, `port-forward-<Svc>.log`; `tempest.conf` excluded — admin password) + dump `OPERATOR=keystone` (+ one `OPERATOR_ONLY=1` pass per compute-stack operator on nova/cinder) |

Gating facts to know before reading results:

- `tests/e2e/chainsaw-config.yaml` runs `parallel: 4` with `failFast: true`:
  everything after the first failure is cascade or never ran, so a red leg
  says nothing about its SKIP suites and each run surfaces one or two causes.
- A job wall (`timeout-minutes`) that arrives before a suite's own timeout
  kills chainsaw outright: no catch block, no JUnit, and the job reads
  **cancelled**, not failed (see `job-wall` below).
- PR CI checks out the merge of the head with current `main`, not the head.
- Suite selection: CI passes suite directories (the chaos job an explicit
  `test_dirs` list). At the pinned v0.2.15, `--include-test-regex` /
  `--exclude-test-regex` **do** filter, with `go test -run`/`-skip`
  semantics over `chainsaw/<metadata.name>`: `'chainsaw/<name>'` selects, a
  bare `'<name>'` matches no top-level test and runs **zero** tests with exit
  0 (verified 2026-09-23 with `chainsaw test --no-cluster` on two probe
  suites). The comment on the `e2e-chaos` job in ci.yaml still calls them
  no-ops at v0.2.14. Locally, pass the suite directory.

## Procedure

### 1. Collect the evidence

```bash
bash .claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh --pr <number>
# or, when you already know the run (optionally an earlier attempt, one job family):
bash .claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh --run <run-id> [--attempt <n>] [--job '<ERE>']
# offline, against a saved log (no gh):
bash .claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh --log-file <path>
```

It lists the failed and cancelled jobs with runner and failed step, and per failed job the skipped jobs downstream of it (from the
local ci.yaml `needs:`). It downloads each job's full log by job id into
`_output/e2e-failure/run-<id>/failed-jobs.log` (ANSI-stripped;
`failed-jobs.nosrc.log` drops the workflow's echo of each step's script),
the check-run annotations into `annotations.txt`, extracts
`chainsaw-excerpt.log`, runs the **signature scan**, maps failed chainsaw
tests to their `chainsaw-test.yaml`, lists which images the job reused from
main by digest versus built in this run, and lists the artifacts. Exit 2
means the run is still in progress.

The scan prints `[MATCH] <pattern> — <meaning> — see SKILL.md § Known
failure patterns` with the first matching line; meanings starting `infra:`
belong to the first table below, `noise:` rows explain log noise and are
never the cause. It also flags a failed job with zero recorded steps as
`runner-lost` from the job metadata alone. `--list-signatures` prints the
table; it lives at the top of the script and is the single place to add one.

### 2. Decide which half you are in

Read the scan's verdict line, then confirm:

- **Infrastructure** when a job has zero steps or no log (the collector's
  `no log` line carries gh's `HTTP 404` from the job-log endpoint), when the
  failed step is `Set up job`, a clone or an image pull/push, when the same
  signature hits unrelated PRs, or when the image provenance shows the
  implicated image reused from an older main. Go to the first table; fix the
  environment, not the tree. A red `Deploy K-ORC` is either half:
  `korc-tag-expired` or `korc-suspend-race`. A `no log` line with any other
  gh error is a failed download, not a lost runner: fix the download and
  collect again.
- **Suite / product** otherwise. Read the chainsaw block (step 3) and the
  dump (step 4), then the second table.

### 3. Read the chainsaw failure block

Work top-down in the excerpt:

- `--- FAIL: chainsaw/<test-name>` is the authoritative marker; the test
  name equals `metadata.name` in the suite's `chainsaw-test.yaml`. With
  `failFast: true`, diagnose the **first** FAIL only.
- The step table (`| HH:MM:SS | <test> | <step> | <OP> | ERROR |`) names the
  step and operation that died; the `SCRIPT … LOG` block with the step's
  `=== STDOUT` sits just **above** the ERROR row. `ASSERT` timeouts print an
  expected-vs-actual diff. chainsaw echoes every script body, so an
  `echo "ERROR: …${var}…"` line with unexpanded variables is source, not
  output.
- `catch:` blocks (and `tests/e2e-chaos/diagnostics.sh`) dump CR conditions,
  pod logs and events right below the failure — usually where the cause
  sits, e.g. a failed startup probe or an `ExternalSecret` `UpdateFailed`.

### 4. Read the diagnostic dump

`hack/ci-dump-diagnostics.sh` prints, in order: HelmReleases, Pods,
DaemonSets, **Node capacity and allocated resources**, **Containers with
restarts** (last termination reason, exit code, QoS class), **Memory working
set per pod**, **Kernel OOM events on the kind node(s)** (dmesg), chaos-daemon
detail, Events (last 50), Flux logs and FluxInstance; then, with `OPERATOR`
set, operator pods and logs (`<op>-system`), Job descriptions and logs, every
pod log in `openstack` (current and previous), the CR's `conditions:` and the
ConfigMaps. `OPERATOR_ONLY=1` passes repeat only the operator sections.
Match timestamps against the chainsaw step table: the question is always
"what was the cluster doing when the assert timed out". Two traps: the dump
runs **after** chainsaw's `finally`, so whatever the suite tore down is gone
(see `evidence-after-finally`); and the pod table shows the current state —
read `OOMKilled` in the restarts block, not in the pod table.

### 5. Classify against the known patterns

Check both tables below before writing a fix. If the failure is a CRD schema
mismatch ("unknown field", "invalid value"), switch to
[[check-fixture-drift]]; if webhook and CRD disagree, to [[check-crd-drift]]
and [[check-validation-parity]].

### 6. Reproduce locally

```bash
make deploy-infra                      # kind + Flux stack; add opt-ins:
# WITH_CHAOS_MESH=true    -> e2e-chaos suites
# WITH_PROMETHEUS=true    -> prometheus-stack suite
# WITH_CONTROLPLANE=true  -> the c5c3 ControlPlane suites (+ K-ORC and the operators)
# WITH_NFS=true / WITH_MESSAGING=true / WITH_OVN_KERNEL_MODULES=true -> cinder / nova / OVN chassis legs
OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator hack/ci-deploy-operator.sh
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/<suite>/
```

What a laptop can host decides the method:

- **Docker Desktop cannot host the full stack.** The author's machine (6 CPU,
  8 GB, linuxkit kernel 6.12) does not fit `make deploy-infra` (RabbitMQ,
  MariaDB, OpenBao, Garage, Envoy, operators), let alone the e2e-controlplane
  or a nova leg. Its kernel has `geneve` and NFS built in but **no
  openvswitch** module and no `/lib/modules`: anything that needs a real
  ovn-controller (chassis suites such as `tests/e2e/ovn/chassis-single-node/`,
  `e2e-ovn-overlay`, port binding, metadata-agent namespaces) runs only on a
  CI leg. `hack/deploy-infra.sh` skips the modprobe off Linux.
- **Narrow throw-away kind repro (~10 min).** `kind create cluster` on its
  own `--kubeconfig` (other projects' kind clusters are not CobaltCore dev
  clusters; never point a suite at the current context blindly), the
  cert-manager static manifest, one operator via `kubectl apply -k` on a
  local kustomization pinned to the repo's Flux ref, one CR. Shape timing
  with `docker exec <node> kill -STOP <pid>` or `docker update --cpus`. For
  chainsaw assert logic, apply only the CRD and simulate the operator's
  status with `kubectl patch --subresource=status --type=merge`. K-ORC alone:
  a throw-away cluster + `hack/ci-deploy-korc.sh` (~4 min). A Keystone-level
  race can be cheaper still in plain docker (two `ghcr.io/c5c3/keystone`
  containers on sqlite + one memcached reproduced `shared-memcached-401` in
  ~2 min). Delete the cluster afterwards.
- **When the leg cannot run locally**, verify statically
  (`make chainsaw-lint`, extract the suite's scripts and `bash -n` /
  `shellcheck` them, run the presence-guard path) and say plainly that the
  live leg was not run.
- **Hand-built labs:** a Tempest run in a pod needs its `tempest.conf`
  `log_dir` to exist (else `stestr` exits 100 with `ArgsAlreadyParsedError`);
  `nohup … &` inside `kubectl exec` dies when the exec returns — run the
  suite as the command of a `restartPolicy: Never` pod that writes rc/JUnit,
  then `sleep infinity`, and poll a marker file; RabbitMQ on an `emptyDir`
  loses its vhosts and users on restart — recreate them before blaming the
  service; a nova `fake.FakeDriver` cannot restart (random node uuid) — use
  `fake.FakeDriverWithoutFakeNodes` with a preseeded `compute_id` on every
  fake compute. List `docker images` before a prune next to other projects.
- `make test-shell` is red on this laptop on `main` too, in the same three
  suites (`tests/unit/hack/deploy_infra_preflight_test.sh`,
  `tests/unit/hack/deploy_infra_reconcile_sources_test.sh`,
  `tests/unit/renovate/fluxoperator_custommanager_test.sh`); CI passes. Run
  `tests/unit/<area>/` and compare `Results:` lines against a `main`
  worktree before calling a failure yours.

Family-specific constraints:

- `make e2e-chaos` / `e2e-prometheus` / `e2e-controlplane` /
  `e2e-controlplane-sso` carry preflights that name the missing opt-in —
  trust their remediation hint.
- `make e2e-operator-upgrade` must run against a cluster **without** a
  pre-deployed keystone-operator (the suite installs the released baseline).
- `e2e-multicluster` needs the two-cluster stack first:
  `INFRA_ONLY=true CLUSTER_NAME=cobaltcore-target make deploy-infra`, then
  `hack/deploy-mgmt-cluster.sh` (leaves the kubectl context on the management
  cluster), the operators via `hack/ci-deploy-operator.sh`, and the target
  registration from `docs/guides/deploy-to-a-target-cluster.md`.
  `make e2e-multicluster` preflights each prerequisite separately (management
  context, the `cobaltcore-target` Secret in `c5c3-clusters`,
  `_output/cobaltcore-target.kubeconfig`).
- After an OpenBao pod kill, run `tests/e2e-chaos/unseal-openbao.sh` — the
  single-replica kind topology has no auto-unseal.

### 7. Fix with the right shape

First, rerun or fix? A **one-off infrastructure death** is the one case
where `gh run rerun <id> --failed` is right and nothing is committed: a
`runner-lost` job (zero steps, "lost communication" annotation, runner
offline in `gh api repos/C5C3/cobaltcore/actions/runners`), a single
`ghcr-transient` timeout. Everything else that recurs — the same signature on
a second run, a class of failure hitting a different victim suite each time
(an OOM kill, a race) — gets its root cause fixed, **in its own issue and PR
against `main`**, not by a rerun (one green at ~40 % teaches nothing) and not
by a commit inside the feature PR that surfaced it (it rides on that PR's fate
and vanishes from its walkthrough). Diagnose to the mechanism with evidence
(restart trail, QoS class, what the PR does and does not touch, runner
correlation, the tree diff between the last green and the red run), name the
files the fix reaches, and put the landing to the author with "own issue + PR
against main" as the recommendation. Ship the diagnostics that make the next
occurrence legible from the job log alone with the fix (`1cb0982d` added the
node-pressure blocks with the MariaDB fix). Once it merges, the feature PR
needs only a CI rerun — PR CI tests the merge with main — no rebase. No
placebo commit, no silencing.

Test-side shapes:

- A status flip observed before a dependent object exists → replace the
  single-shot `kubectl get` with a chainsaw `assert` (it polls to the step
  timeout). See `5b0c961a`.
- A genuinely transient boundary (webhook just rolled, registry eventually
  consistent) → a bounded retry loop **plus** an explicit step timeout larger
  than one failed attempt. See `85e3172a`.
- A hard-coded timing window coupled to spec defaults → derive the window
  from the live rendered object and fail loudly when the extraction comes
  back empty. See `0fa60e6e`.
- A JMESPath function over a field the operator fills in later → guard it:
  ``length(x || `[]`)``, ``contains(finalizers || `[]`, …)``.
- Evidence the suite destroys → dump service pods and every pod of a failed
  Job in `catch`, never in `finally` or the workflow's later dump.
- Never widen a timeout or a job wall without a comment stating the budget
  it absorbs (`tests/e2e/chainsaw-config.yaml` `cleanup: 3m` and the wall
  derivations on the e2e-controlplane, e2e-operator and tempest jobs are the
  templates).

## Known failure patterns

The Pattern column is the name the collector's signature scan prints; `—`
means the pattern has no log signature and is recognised by hand.

### Infrastructure — not the code

| Pattern | What you see | Cause | Action |
|---|---|---|---|
| `runner-lost` | a failed job with **zero** steps, no log (job-log endpoint 404), ~10 min wall, annotation "The self-hosted runner lost communication with the server"; a sibling on the same runner cancelled at `Set up job` ("A task was canceled." / "The runner has received a shutdown signal") | a self-hosted runner died mid-assignment | runner name from `gh api repos/C5C3/cobaltcore/actions/jobs/<id> --jq .runner_name`; `gh run rerun <id> --failed` re-queues the job and re-evaluates its skipped dependents — a lost `test-integration` leg silently skips `build-e2e-images` and every e2e job behind it. No commit |
| `anon-clone-401` | `fatal: could not read Username for 'https://github.com'` then `fatal: expected flush after ref listing` in a cloning step (service image build, `Deploy K-ORC`) | GitHub intermittently answers an unauthenticated protocol-v2 upload-pack from git < 2.45 (the runners ship 2.43) with a 401 | authenticate the clone: `GITHUB_TOKEN` as `http.https://github.com/.extraheader` via `GIT_CONFIG_COUNT/KEY_0/VALUE_0` (`hack/ci-build-service-image.sh`, `hack/ci-deploy-korc.sh`; `images/ovn/Dockerfile` takes it as the `github_token` BuildKit secret), `0c7d6e9a`; `tests/unit/ci/github_git_auth_wiring_test.sh` fails any cloning step without the env. Never two Authorization headers (400 "Duplicate header") |
| `korc-tag-expired` | `e2e-operator (c5c3)`, `e2e-controlplane`, `-sso` and `e2e-external-keystone` red together in `Deploy K-ORC`; orc-system pod ImagePullBackOff on `quay.io/orc/openstack-resource-controller@sha256:…: not found` | quay expires `commit-<sha>` tags after four weeks and drops the digest pinned in `deploy/flux-system/releases/k-orc.yaml` | `curl -s 'https://quay.io/api/v1/repository/orc/openstack-resource-controller/tag/?specificTag=commit-<short>'` shows `expiration`; re-pin commit, `newTag` and `digest` in lockstep with [[bump-korc-pin]] |
| `stale-image` — | a PR fails on behaviour main already fixed; the image provenance shows the implicated image pulled `@sha256:` (reused); `build-e2e-images` `Image map` says `(reused)` | a `build-and-push` leg failed on the main push, `merge-operator-images` skipped for every operator, `:latest` kept an older revision; a later push republishes only operators whose sources changed | since `8347f30e` the resolver builds an operator image whose revision lacks commits to its sources, so PR legs self-heal; `:latest` itself (and the `e2e-operator-upgrade` baseline) stays stale until `gh run rerun <main-run> --failed` — within a day, the digest artifacts have `retention-days: 1`. Check the `org.opencontainers.image.revision` label of `latest` on GHCR |
| `ghcr-transient` | `ghcr.io … (Client.Timeout exceeded while awaiting headers)`, `docker login/push … failed after 5 attempts` | registry eventual consistency, transient 5xx | bounded retries `4b3cfbde`, `1f43aee7`, `f67318cd`; GHCR transport `b5efce28`. Once: rerun; recurring: own issue |
| `go-merge-with-main` | `verify-codegen` → `FAIL: operators/<op> is not tidy` (`make verify-go-tidy`) or a `go.work.sum` diff; `test (<op>)` `[setup failed]` on `verifying go.mod: reading https://sum.golang.org/…`; nothing reproduces on the branch | PR CI tests the merge with main: a module lagging a bump main carries loads the unpruned graph and fetches checksums | reproduce in a worktree merged with `origin/main`; bump the lagging `go.mod` to lockstep and `go mod tidy` **in the merged tree** (`go get` indirect pins explicitly); a module new on the branch needs its own tidy. [[check-go-workspace-deps]] |
| `kernel-modules` — | NetworkChaos suites fail on missing `ip_set` / `xt_set` / `sch_netem`; OVN chassis pods without a datapath | the runner kernel lacks the modules | `hack/deploy-infra.sh` installs `linux-modules-extra-$(uname -r)` and retries; OVN modules load only under `WITH_OVN_KERNEL_MODULES=true`; the `network`, `ovn`, `nova` chaos legs and `e2e-ovn-overlay` stay `continue-on-error` |

### Suite / product

| Pattern | What you see | Cause | Action / reference fix |
|---|---|---|---|
| `job-wall` | job **cancelled**, annotation "The job has exceeded the maximum execution time of <wall>"; no catch output, no JUnit | bring-up + passing suites + the stalled suite's ceiling exceed `timeout-minutes` | the last suite or step started in the log is the one that stalled; re-derive the wall from measured bring-up as the ci.yaml comments do, never raise it bare |
| `webhook-stale-keepalive` | `Error from server (InternalError): … failed calling webhook` / `context deadline exceeded` on the first webhook-gated kubectl right after an operator rollout | apiserver reuses a stale keep-alive to a terminated webhook pod IP | `85e3172a` — retry loop with fresh dials + 120s step timeout |
| `oneshot-get-race` — | single-shot `kubectl get <job/pod>` fails although the object appears seconds later | the script raced the controller's next pass (status flips before the object exists) | `5b0c961a` — chainsaw `assert` instead of a one-shot get |
| `kube-proxy-lag` — | in-cluster HTTP probe refused seconds after the CR flipped Ready, every pod serving | kube-proxy's endpoint programming trails the Ready flip | `d11fef10` — retry connection-level errors (15 × 2s) + 90s step timeout; `HTTPError` still fails hard |
| `maintenance-endpoint` — | `ECONNREFUSED` on the API Service when a maintenance CronJob fires; a `*-trust-flush-*` / `*-db-purge-*` pod in the catch dump at that moment | the maintenance pod's labels satisfied the API Service selector — a product bug, not a flake | `1e734cc0` — `component=api` + two-phase selector narrowing; `tests/e2e/keystone/maintenance-endpoint-isolation` pins it |
| `namespace-hijack` | `Error from server (NotFound): pods "openbao-0" not found` from a helper script | chainsaw injects `NAMESPACE=<test ns>` into every script step | `2c23003e` — dedicated `OPENBAO_NAMESPACE` contract |
| `hardcoded-window` — | availability-sampling loop flakes once a CR raises `terminationGracePeriodSeconds`/preStop | window silently coupled to spec defaults | `0fa60e6e` — derive the window from the rendered Deployment |
| `cleanup-timeout` — | cleanup timeout in deletion suites under `parallel: 4` | the MariaDB operator serializes Database/User/Grant deletions | `cleanup: 3m` in `tests/e2e/chainsaw-config.yaml` |
| `openbao-sealed` — | OpenBao pod 0/1 Running forever after a chaos kill | single-replica Shamir sealing, no auto-unseal on kind | `tests/e2e-chaos/unseal-openbao.sh` |
| `korc-suspend-race` | `Deploy K-ORC` fails `Apply failed with 1 conflict: conflict with "kustomize-controller": …containers[name="manager"].image` | Flux reconciled Kustomization `k-orc` before deploy-infra suspended it (Flux renders `<name>:<tag>@<digest>`, the script `<name>@<digest>`) | `f0eb0067` — `deploy/kind/base/kustomization.yaml` applies it suspended, pinned by `tests/unit/deploy/kind_base_korc_suspend_test.sh`. If it returns, compare the `k-orc created` / `patched` timestamps in the deploy-infra log; never `--force-conflicts` |
| `rabbitmq-recreate-race` | messaging suite: `rabbitmqcluster/cp-rabbitmq was not removed within 180s`; a same-name RabbitmqCluster with no ownerReferences, Generation 1, created a second after the owned one was GC-deleted | upstream cluster-operator `removeFinalizer` runs `CreateOrUpdate` on an empty object, so a stale reconcile re-creates the broker (unfixed upstream as of 2026-09-06) | `ab57392e` — the c5c3 teardown deletes the bus with foreground propagation and holds its finalizer (`InfrastructureReady=False/FinalizingMessaging`, 3m `messagingTeardownDeadline`, Warning `MessagingTeardownStalled`); if it returns, check the guard ran before blaming the suite |
| `mariadb-oom`, `oom-kill`, `db-sync-1054` | a suite misses its assert with its CR at `DatabaseReady=False` (`ClusterNotReady` / `DBSyncInProgress`), a different victim suite each run; `openstack-db-0` restarts, an **empty** `Liveness probe failed:`, "Recovering after a crash"; `OOMKilled` in the restarts block; a neutron db-sync looping on `(1054, "Unknown column …")` | kernel OOM kills of the kind MariaDB under node memory pressure; a connection lost mid-DDL leaves the schema half-migrated, so every retry fails the same way | `a8b204fb` — 1Gi memory request = limit on the kind MariaDB patch, no CPU (`tests/unit/deploy/kind_mariadb_resources_test.sh`); `1cb0982d` — node-pressure blocks in the dump. If it returns, read those blocks first; never a CPU request (it starved the keystone leg into `Insufficient cpu`). For 1054 read the **earliest** db-sync pod. `cinder-backup` OOM against its 2Gi limit is #1003 (open) |
| `jmespath-nil` | `Internal error: invalid type for: <nil>, expected: […]` with RUN and ERROR in the same second; failFast then skips the rest | chainsaw aborts at its first poll when a JMESPath function (`length`, `contains`, `join`, `keys`) gets an absent field under an **existing** parent (an absent parent is a retryable "field not found") | guard the argument, ``length(x \|\| `[]`)`` (house idiom in `tests/e2e/horizon/deletion-cleanup`, `tests/e2e/cinder/deletion-cleanup`) |
| `evidence-after-finally` | verify Job red but the catch shows only the retry pod; later service-pod tails all `NOT_ALLOWED - vhost <x> not found` | `kubectl logs job/<name>` resolves to the newest pod; the workflow's dump runs after chainsaw's `finally` removed the vhost | read every pod `-l job-name=<job>` (`-o name` is alphabetical — check creation time); dump service pods in `catch` (`13a2723c`); a `<none>` from a name-based `show` after a Job retry means a duplicate name |
| `shared-memcached-401` | nova-conductor/scheduler crash-loop on `keystoneauth1.exceptions.http.Unauthorized` building the Placement client, Nova `NotAllReady`; passes when only one Keystone suite is live | two Keystone CRs on one Memcached share the `get_user_by_name` cache entry (`internal/common/cache/cache.go` resolves `cache.clusterRef` to `<name>:11211`, no instance scope; `key_prefix` does not isolate) | a suite that brings its own Keystone brings its own `Memcached` (`<keystone>-cache`, see `tests/e2e/nova/*/00-keystone-cr.yaml`) |
| `nova-bring-up`, `aborted-connection` | Nova suites time out at 5m; MariaDB logs "Aborted connection … (Got an error reading communication packets)" every ~25 s | a Nova needs ~8 min to Ready: eight MariaDB CRs at a 30 s requeue per stage, then six ~25 s nova-manage runs; the aborted connections are nova-manage exiting (progress, `noise:` in the scan) | 10m on `DatabaseReady` inside a 15m envelope (`tests/e2e/nova/gateway-quick-start-smoke`); one `openstack` CLI call costs ~12.5 s on the e2e node — budget a catalog Job by its call count; a script without `timeout:` dies at the config's `exec: 30s`; the db-sync pod lacks the instance label, read the events of `job/<nova>-db-sync` |
| `catalog-import-race`, `korc-log-selector` | e2e-external-keystone `ERROR: all catalog imports resolved — expected '4', got '3'`; catch blocks print "No resources found in orc-system namespace" | admin/internal Endpoint imports are best-effort and K-ORC resolves them after the Service flips Available; the catch's K-ORC selector `app.kubernetes.io/name=openstack-resource-controller` matches nothing | `c9899d76` — `wait_cp_imports_resolved` (120s); check the K-ORC Endpoint CRs in the catch output; K-ORC's own logs are not captured until that selector is fixed |
| `insufficient-cpu` | `FailedScheduling … nodes are available: … Insufficient cpu`, pods Pending, CRs never Ready | node CPU request budget exceeded (four neutron/cinder/nova suites at once; a new request elsewhere) | read "Node capacity and allocated resources" first — runner hosts differ, compare `runner_name`; those three legs run `--parallel 2`; the e2e-controlplane node measured 8 CPU / 32 GiB with 56 % CPU requested at seven services (job 104129337854, 2026-09-14) — re-measure from a current job log |
| `tempest-port-forward` | annotation "The <Svc> port-forward on <port> died during the run" | the port-forward died; failures against `localhost:<port>` after it are the forward | `port-forward-<Svc>.log` in the results artifact |
| `tempest-race` — | one tempest test fails intermittently in one release row | upstream test races under concurrency | `ca194368` — failed tests rerun once serially and are rewritten as flakes in the JUnit |

## Notes

- Read-only: the collector issues GET requests through `gh` and writes only
  under `_output/e2e-failure/` (`_output/` is gitignored). Fixes are a
  separate, explicitly scoped task.
- Flake fixes land as their own commit whose message names the race and why
  the fix bounds it (see `85e3172a` for the calibration).
- Suites live directory-per-suite and are auto-discovered — see
  `tests/e2e/README.md` and `tests/e2e-chaos/README.md`; a new **chaos**
  suite must also join a `test_dirs` list in ci.yaml. Suites that need a
  different cluster shape live outside `tests/e2e/` on purpose
  (`tests/e2e-multicluster/`, `tests/e2e-controlplane-sso/`,
  `tests/e2e-operator-upgrade/`, `tests/e2e-ovn-overlay/`): `make e2e` and the
  `e2e-operator` legs sweep `tests/e2e/`.
- A new known pattern goes into both places: a row in the tables above and,
  when the log carries a stable literal, a `SIGNATURES` row under the same
  name at the top of the collector
  (`.claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh`).
  Write the ERE so it cannot match script source (chainsaw echoes script
  bodies; ci.yaml `run:` blocks carry comments) and check it against the
  suites:
  `find tests -name chainsaw-test.yaml -exec cat {} + | grep -cE '<ERE>'`
  should print 0.
- Pair with [[check-fixture-drift]] for schema mismatches,
  [[check-crd-drift]] when the webhook rejects what the CRD allows,
  [[check-go-workspace-deps]] for workspace skew, and [[bump-korc-pin]] for
  an expired K-ORC image.
