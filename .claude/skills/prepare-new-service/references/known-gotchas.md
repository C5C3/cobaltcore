# Known gotchas (verified 2026-07 post-glance; refreshed 2026-09-23 at `12202e40` after the Neutron/OVN, Cinder and Nova onboardings, re-verify at HEAD)

Read this in step 1 while profiling (the image and runtime entries decide
Phase 0 spike items) and in step 5 before writing the checkboxes: each
entry names the layer and phase it bites in. Issue numbers point at the
run that paid for the lesson; the lesson itself is the part to carry.

## Layer 1 — container image

- **upper-constraints pin conflict:** some services (horizon, most
  clients' dashboards/libraries) are already pinned in
  `releases/*/upper-constraints.txt`. Installing from source with
  `--constraint` then requires the source ref to match the pin exactly,
  or a `-<svc>` line in `overrides/<release>/constraints.txt`
  (`scripts/apply-constraint-overrides.sh`). Check the service's
  **libraries** too, not just the service: glance itself is unpinned, but
  `glance_store`/`boto3` are — the driver extra must resolve against the
  existing pins.
- **hadolint matrix is static** in `build-images.yaml` — new Dockerfiles
  must be added by hand even though the build matrix auto-discovers. So
  are the changes job's `ALL_SERVICES` list, the `svc_<svc>` paths filter
  and the `FILTER_svc_<svc>` env line (plus the option-catalog trigger
  lists when the build re-derives the service's catalog);
  `tests/unit/ci/build_images_services_lockstep_test.sh` fails when one is
  missing.
- **Image patches need upstream test hunks:** `test-service-images (<svc>,
  <release>)` clones the upstream project at its ref, applies
  `patches/<svc>/<release>/*.patch` and runs the project's **own unit
  suite** against the result, so a patch that changes behavior turns that
  job red unless the same `.patch` also updates every upstream test that
  asserts the old behavior (cinder `0002`, #995). Say in the patch header
  which tests move and which keep the old value ([[add-image-patch]] walks
  the whole patch contract). `verify_<svc>.sh` runs
  elsewhere: `build-service-images` passes `verify-script:` only on a pull
  request, and the separate `verify-service-images` job is skipped on
  pull requests, so a green `build-service-images` is the proof a new
  verify test passed.
- **Probe both releases in the image before Phase 1 closes:** the upstream
  unit discovery and `hack/gen-option-catalog.sh` import the service's
  modules inside the published image, and an old release can import
  something the current toolchain dropped (cinder 27.0.0's Windows drivers
  pull `os_win` → `pkg_resources`, gone from setuptools ≥ 81; hence the
  `setuptools<81` pins in `hack/ci-run-unit-tests.sh` and
  `images/cinder/Dockerfile`). Some imports also need registration first
  (cinder's NFS driver needs `cinder.objects.register_all()`), so write the
  verify script's import tests against the real module graph.
- **Building a service image on macOS:** `hack/ci-build-service-image.sh`
  dies after the clone on `scripts/apply-constraint-overrides.sh`'s GNU
  `sed -i "/^pkg===/Id"`. Reproduce CI's image by hand instead: run the
  override script in a `python-base` container on copies of
  `releases/<rel>/upper-constraints.txt` and
  `overrides/<rel>/constraints.txt`, clone the tag from
  `releases/<rel>/source-refs.yaml` (apply `patches/<svc>/<rel>/*.patch`),
  then `docker build --build-arg EXTRA_APT_PACKAGES=<from
  extra-packages.yaml> --build-context <svc>=<src> --build-context
  upper-constraints=<dir> images/<svc>/`; `--target <stage>` builds one
  stage without the named contexts.
- **Hand-maintained per-service lists in the verify/matrix scripts:**
  `tests/container-images/verify_release_config.sh` (`SERVICES=` list),
  `verify_deviation_comments.sh` (`SERVICES=` list, plus an own function
  for an image not built on `python-base`),
  `hack/ci-generate-tempest-matrix.sh` (`ALL_TEMPEST_SERVICES`, mirrored by
  `TEMPEST_ALL_SERVICES` in `hack/ci-resolve-changes.sh`), and the
  chaos CI job's image-load lists in `ci.yaml` are all generalized now but
  still enumerate services by hand — extend each one, or the new
  service's coverage silently never runs. The rest of the literal copies
  (the Makefile `gen-option-catalogs` / `verify-option-catalogs` loops,
  `write-bootstrap-secrets.sh`, the `hack/deploy-infra.sh` tenant wait
  set, `tests/lib/ci_resolve.sh` and the `tests/unit/ci` fixtures) are
  found by `git grep -nE 'neutron[ ,]+cinder[ ,]+nova' -- ':!docs' ':!*.md'`;
  32 hits on 2026-09-23, Go comments and test literals included.
- **The parameterized `operators/Dockerfile` (`ARG OPERATOR`) is still
  coupled to `go.work`:** it COPYs every module's go.mod and source, so a
  new module still edits that one Dockerfile's COPY lines (no more
  per-operator Dockerfiles, though).
- **WSGI entry points:** `uv pip install --prefix` skips PBR
  `wsgi_scripts` generation — service Dockerfiles hand-write their WSGI
  launcher (see `images/keystone/Dockerfile`). Also verify the stock WSGI
  module actually honors `--config-dir`/`--pyargv`: glance's
  `glance.wsgi.api:application` reads only its default config path and
  needed a hand-shipped shim (`images/glance/glance-wsgi-api`) to load
  the operator's mounted config dirs. Nova reads its files from
  `OS_NOVA_CONFIG_DIR` / `OS_NOVA_CONFIG_FILES`, and an absolute first
  entry (`/var/lib/openstack/etc/nova/api-paste.ini;nova.conf`) serves
  `api-paste.ini` without a copy. Mind what `--config-dir` parses: every
  `*.conf` in it, which is why cinder names its logging file
  `logging.ini`.

## Layer 2 — service operator

- **A CronJob shrinks the CR's name budget:** Kubernetes caps a CronJob
  name at 52 characters (`MaxCronJobNameLength` in
  `operators/glance/api/v1alpha1/glance_webhook.go`), so
  `{name}-<suffix>` bounds `metadata.name` itself. Enforce that bound
  **on create only** — `metadata.name` is immutable, so on update the
  rule can only fire against objects a pre-bound operator already
  admitted, including the finalizer-removal update that completes a
  deletion, wedging them permanently. The controller therefore stays
  total for over-long names by collapsing them onto a content-stable
  hash (`dbPurgeCronJobName`). Adding the first CronJob to an existing
  operator means paying all of this; adding it during onboarding means
  the bound exists before any CR does.
- **CEL transition rules under a defaulted parent need `has()`:** a
  struct-level rule (`self.x == oldSelf.x`) on a type that a parent field
  defaults with `+kubebuilder:default={}` makes the CRD uninstallable —
  the API server validates the rule against the `{}` default and fails
  with `no such key`. Guard both sides
  (`!has(self.x) || !has(oldSelf.x) || self.x == oldSelf.x`, the ovn
  shape). Unit tests never catch it; only envtest or a `kubectl apply` of
  the CRD does.
- **The defaulting webhook masks `required`:** on a cluster that serves
  the defaulting webhook, an omitted required field whose Go type is a
  non-pointer struct (or string) without `omitempty` is materialized by
  the webhook's typed round trip (`messaging: {}`, `openStackRelease:
  ""`), so the next rule answers (the MessagingSpec XOR CEL, the CRD
  pattern), never `Required value` — that message shows only on the
  webhook-less envtest. `tests/e2e/placement/invalid-cr/00-openstackrelease-missing.yaml`
  has the right wording. Chainsaw runs fail-fast, so one wrong
  expectation hides every later step: check a new invalid-cr corpus
  without a cluster through a throw-away envtest (webhook served) that
  applies each fixture and matches the `contains($error, '…')` strings.
- **Schema defaults land before the webhook:** `commonv1.DeploymentSpec.Replicas`
  carries `+kubebuilder:default=3`
  (`internal/common/types/workload.go`), applied at decode whenever a
  `deployment` block is present. A singleton workload embedding it
  (cinder-volume, cinder-backup) must be stated `replicas: 1` by the CR
  and by the ControlPlane projection, since typed Go clients always
  serialize the block.
- **`RunParallelGroup` drops status writes:** each member gets its own
  `cr.DeepCopy()`, and only the member's condition (`MergeCondition`)
  and persisted metadata (`adoptMetadataWrites`) come back. Any other
  `status.*` field a member writes is discarded silently (ovn's
  `status.installedImage` and `status.relayAddress`). Carry such a field
  onto the primary CR inside the step's `Fn` and pin it with a test that
  goes through `step.Fn(ctx, primary.DeepCopy())`; unit tests that call
  the sub-reconciler directly never take that path.
- **`unused` lint blocks staged symbols:** `golangci-lint` runs `unused`
  with no per-path exclusion, so a constant, field or helper declared in
  an early commit "for later" fails that commit. Declare each symbol
  beside its first consumer (`glanceAppName` in `reconcile_deployment.go`)
  and use string literals in maps (the instrumentation map) until the
  constants land; tell each implementer which symbols do not exist yet.
- **gosec G204 in tests:** `.golangci.yml` excludes only G101 from test
  files, so a test that runs a shell script through
  `exec.CommandContext` must keep it a `const` (cinder splits
  `upgradeCheckScriptHead` / `upgradeCheckScriptTail` for exactly this).
- **Security contexts and shared sockets:** `CapabilitySecurityContext`
  drops ALL, `CAP_DAC_OVERRIDE` included, so a `RunAsUser: 0` container
  is an ordinary user against file modes. A container sharing a Unix
  socket or directory with a `RestrictedSecurityContext` (uid 42424)
  sibling needs `RunAsGroup: 42424` plus group-writable modes on both
  sides (`install -d -m 0775`, `umask 002` before the daemon that creates
  the socket); connecting needs the **write** bit. A container waiting in
  `until <cmd> >/dev/null 2>&1; do sleep 1; done` logs nothing at all, so
  "Running, not ready, zero output" points at the wait loop.
- **Privileged workloads constrain the chart and the namespace:** the ovn
  and neutron charts fail the render on `rbac.namespaceScoped=true` (the
  operator-library chart hook), and the Neutron metadata agent lives
  beside the `OVNChassis` in the privileged OVN namespace as a CRD of its
  own rather than as a cross-namespace child of `Neutron` (#904). Decide
  the namespace model in Phase 0 when anything needs host access.
- **Size `max_user_connections` from measurement:** the mariadb-operator
  CRD default of 10 is below any real topology. Below the cap the last
  processes fail their pool with MySQL error 1226 and `--need-app`
  crash-loops the pod; under load the request past the cap answers 500.
  `neutronMaxUserConnections` (pool per API process plus one per uWSGI
  thread beyond the first, the HPA ceiling, one surge pod, the worker
  Deployments, transient jobs) and `novaMaxUserConnections` (per block)
  are the worked formulas; brownfield `User` fixtures need the same sizing.
- **uWSGI cold start vs. liveness:** nova's WSGI cold start measured
  40–78 s against a 55 s liveness kill; the HTTP front ends carry a
  30×10 s startup probe (`novaUWSGIStartupProbe`, the siblings' shape).
- **Never render a placeholder DSN a role does not override:** nova reads
  a non-empty `[api_database] connection` as "API database reachable",
  so the console proxy dialed host `placeholder` on every token until its
  overlay rendered an empty `connection =`.

## Layer 3 — CI, e2e, deploy

- **In-cluster API probes must retry the connection, not the response:**
  every `basic-deployment` suite fires a one-off pod at the service API
  right after the CR flips Ready, and kube-proxy's endpoint programming
  can trail that flip by a second or two — a single-shot request loses
  that race with every pod serving. Copy the hardened idiom (`d11fef10`):
  retry connection-level failures inside the probe pod (15 attempts, 2s
  apart) with a step timeout covering the whole budget, while `HTTPError`
  and the status/body assertions still fail on the first response, so the
  suite tolerates the endpoint lag and nothing else. Check what `GET /`
  answers first: Cinder answers **300 Multiple Choices** with its version
  document, so every probe and the gateway smoke accept exactly 300
  (sweep `nip.io/`, `http_code` and `expected 200` too, not only the port).
- **Paste-deploy divergence between releases:** factory references
  (`API.factory` vs `API_factory`) and oslo.middleware healthcheck
  semantics (filter tolerated in 2025.2, app-only in 2026.1) differ
  between the pinned releases — pin the rendered paste config in unit
  tests and run the e2e/tempest legs against both releases.
- **Budget the bring-up, not the suite:** a Nova needs about 8 minutes to
  Ready on the kind e2e leg — eight MariaDB CRs at the shared flow's 30 s
  requeue per stage plus a db-sync chaining six `nova-manage` calls at
  ~25 s each — so its suites assert Ready at 10 minutes. One "Aborted
  connection" per ~25 s from the db-sync pod in the MariaDB log is
  `nova-manage` exiting, i.e. progress. One `openstack` CLI call costs
  ~12.5 s on the e2e node, so a catalog or seed Job is budgeted by its
  call count. Chainsaw `failFast` skips every suite after the first
  failure, so a red leg says nothing about its SKIPped suites and each
  run surfaces at most one or two causes.
- **A suite that brings its own Keystone brings its own Memcached:** two
  Keystones on one Memcached poison each other's identity cache (the
  keystone operator sets no per-instance key scope, and `key_prefix` does
  not isolate), so the second answers 401 for `admin` and the dependent
  service crash-loops on `Unauthorized` — a race that looks like a flake.
  The nova suites carry a per-suite `<keystone>-cache` Memcached CR.
- **Broker outages flip readiness slowly:** an `*-amqp-ready` probe that
  reads the socket table sees the connection `ESTABLISHED` until the oslo
  heartbeat (60 s) gives up, so a broker-outage suite budgets the
  readiness flip at 180 s (cinder and nova). Two instances of a service on
  one vhost share RPC topics: give each suite its own vhost
  (`tests/e2e/cinder/broker-vhost.sh` on the `WITH_MESSAGING` broker).
- **Does it fit beside the stack?** Answer from the `=== Node capacity
  and allocated resources ===` block `hack/ci-dump-diagnostics.sh` writes
  into a recent `e2e-controlplane` job log (that job runs on pull
  requests only), not from an old figure: 8 CPU / 32 GiB with 56 % of the
  CPU requested after seven services on 2026-09-14. Runner hosts differ;
  compare the `runner_name` first.
- **`cleanup-e2e-tags (<svc>-operator)` is red by design** on the first
  PR of a new operator (GHCR refuses to delete the last tagged version;
  the job is `continue-on-error`).
