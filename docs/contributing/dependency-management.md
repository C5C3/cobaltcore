---
title: Dependency Management
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Dependency Management

Two related processes that touch the same parts of the repository: bumping the
Go toolchain across the workspace, and keeping third-party libraries, container
base images, and GitHub Actions current. Renovate runs continuously and opens
grouped PRs; this guide is the human-side rulebook for what Renovate does on
its own, what needs a reviewer, and what is better handled in a dedicated PR.

The authoritative configuration lives in [`renovate.json`](https://github.com/c5c3/cobaltcore/blob/main/renovate.json)
at the repository root.

---

## Renovate at a glance

Renovate is configured via `config:recommended` plus a small set of custom
regex managers and package rules. The high-level split is:

| What Renovate does on its own                                                    | What needs a human                                            |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| Opens PRs for every dependency it understands (Go, Docker, GitHub Actions, npm). | **Merging** native-manager PRs — Go/Docker/Actions/npm bumps are opened but not auto-merged. |
| Auto-merges the custom-regex–managed pins and the `gateway-helm` and `headlamp` chart floors (patch/minor) after a 3-day cooldown, see below. | Reviewing major-version bumps.                |
| Maintains Docker image digests (`@sha256:…`) alongside floating tags.            | Reviewing anything touching coupled stacks (k8s).             |
| Pins GitHub Actions to commit SHAs (annotated with a `# vX.Y` tag).              | Updating the Go workspace / `go.mod` directives; triaging CVEs. |

Renovate combines the `config:recommended` native managers (Go modules, Dockerfiles,
GitHub Actions, npm), the native `flux` manager over the Flux manifests, and the
custom regex managers (`jq '.customManagers | length' renovate.json` counts them) that
track pins those native managers cannot see. Auto-merge (patch/minor, 3-day cooldown)
is wired onto the custom-regex managers and, among the native managers, **only** onto
the two floor-tracked Flux charts,
`gateway-helm` and `headlamp` (see [Flux HelmRelease chart versions](#flux-helmrelease-chart-versions)).
Every other native-manager PR waits for a human merge (`config:recommended` does not
auto-merge, and no top-level `automerge` is set).

Renovate performs the merge itself: `platformAutomerge` is set to `false` at the top
level, so an automergeable PR never goes to GitHub's own auto-merge. Renovate merges
it once every check on the PR head has succeeded, because `ignoreTests` is left unset.
Note for operators: `main` currently defines no required status checks, and GitHub's
auto-merge waits for required checks alone. Tightening branch protection is a
repository-settings change and cannot be made from files in this repository.

The custom managers cover:

- **OpenStack release refs** — git tags in `releases/*/source-refs.yaml` and PyPI pins in `releases/*/test-refs.yaml`.
- **Test tooling in `hack/`** — Chainsaw, Flux CLI, kind, and kubectl versions in `hack/install-test-deps.sh`, plus `FLUX_OPERATOR_VERSION` and `ENVOY_GATEWAY_VERSION` (the Envoy Gateway CRD pin, grouped with the `gateway-helm` chart) in `hack/deploy-infra.sh`.
- **kind deploy components** — `flux-web.yaml` under `deploy/kind/base/`.
- **K-ORC Flux source** — the `ref.commit` of the K-ORC `GitRepository` in `deploy/flux-system/sources/k-orc.yaml` (git-refs on upstream `main`, digest updates). This closes a drift gap: without it the Flux-applied K-ORC CRDs could fall behind the `k-orc/openstack-resource-controller` Go module the operator compiles against. A commit bump is not complete on its own: the `commit-<short sha>` image tag and digest in `deploy/flux-system/releases/k-orc.yaml` and the Go pseudo-version in `operators/c5c3/go.mod` move in the same change, and the `hack/ci-deploy-korc.sh` drift guard fails CI until the image is re-pinned.
- **Go build tooling in `Makefile` / `.github/workflows/*.yaml`** — `gofumpt`, `controller-gen`, `golangci-lint`, and `yq`, plus the envtest Kubernetes minor (`ENVTEST_K8S_VERSION`), which follows the `envtest-vX.Y.Z` releases of `kubernetes-sigs/controller-tools` that `setup-envtest` downloads its assets from.
- **`renovate-config-validator` pin** — the `RENOVATE_VALIDATOR_VERSION` constant in `tests/unit/renovate/`, the Renovate release `test-shell` downloads and executes to validate `renovate.json`.
- **OVN image pin** — the `ARG OVN_VERSION` line in `images/ovn/Dockerfile` (github-tags on `ovn-org/ovn`, regex versioning because the 26.03 line carries a leading zero and a `v` prefix). A second manager tracks the same upstream tag in `defaultOVNVersion`, the constant in `operators/ovn/internal/controller/image.go` that the ovn-operator resolves for a CR leaving `spec.image` unset. It carries the bare version, so its versioning regex expects no `v` and an `extractVersionTemplate` strips the one the tag has. Both pins are grouped under `OVN LTS patch releases`, so they move in a single PR; `TestDefaultOVNVersionMatchesDockerfilePin` fails when they diverge.
- **openstack-hypervisor-operator image pin** — the `ARG HVO_COMMIT` line in `images/openstack-hypervisor-operator/Dockerfile` (git-refs on upstream `main` of `cobaltcore-dev/openstack-hypervisor-operator`, digest updates, like the K-ORC source). The rule runs on the weekly schedule (`before 6am on monday`) behind the 3-day cooldown and is **not** automerged. Every upstream `main` commit is a new digest, so without the schedule each one would open a PR. A move also needs a human: a downstream patch may no longer apply, and from #1142 on the lab's chart reference has to move with the pin. A failed `git apply` step in `build-hvo` (`patch does not apply: /patches/<file>`) means that patch has to be re-cut; see [Re-cutting the openstack-hypervisor-operator patch](#re-cutting-the-openstack-hypervisor-operator-patch).
- **kvm-node-agent image pin** — the `ARG KNA_COMMIT` line in `images/kvm-node-agent/Dockerfile` (git-refs on upstream `main` of `cobaltcore-dev/kvm-node-agent`, digest updates), with the same weekly schedule, 3-day cooldown and no automerge as the hvo pin. The lab's chart reference, `ref.tag` and `ref.digest` of the `kvm-node-agent` `OCIRepository` in `deploy/lab/metal-stack/hypervisor/sources.yaml`, has no manager of its own and moves with the pin in the same PR; `tests/unit/deploy/metal_stack_hypervisor_test.sh` fails while its short SHA differs from the pin. A failed `git apply` step in `build-kna` means the patch has to be re-cut; see [Re-cutting the kvm-node-agent patch](#re-cutting-the-kvm-node-agent-patch).
- **noVNC console assets** — the `ARG NOVNC_VERSION` and `ARG NOVNC_COMMIT` lines in `images/nova/Dockerfile` (github-tags on `novnc/noVNC`, regex versioning because the tags carry a `v` prefix). One `matchStrings` entry spans both adjacent lines, so the tag and the commit it names move in a single PR. Majors are disabled; minors and patches wait the 3-day cooldown and are **not** automerged, because the console page is user-facing and no e2e suite loads it before #1018. Digest updates are disabled: a tag moved upstream to another commit is not a release, and the pin stays on the reviewed commit.
- **Lab libvirt image** — the eight `ghcr.io/c5c3/libvirt:<tag>@sha256:<digest>` image lines of the metal-stack lab in `deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml`, `probe/nfs-module-load.yaml`, `nfs/kustomization.yaml`, `nfs/client-modules-daemonset.yaml` and `chaos-mesh/modules-daemonset.yaml` (docker datasource, `deb` versioning). The tag is the keeper tag `<libvirt-package-version>-r<N>`, such as `10.0.0-2ubuntu8.19-r1`, that `hack/ci-tag-libvirt-keeper.sh` mints once on `main`. `deb` compares the Ubuntu revision and the `-r<N>` suffix numerically; `docker` versioning would read `-2ubuntu8.19-r1` as a compatibility suffix and never offer another. `deb` also parses `latest` and a commit SHA and sorts both above `10.x`, so `allowedVersions` admits the keeper shape alone. Majors are disabled, because `tests/container-images/verify_libvirt.sh` pins libvirt 10, the `libvirt0` that `images/nova-compute/Dockerfile` links. Minor, patch and digest updates move all eight lines in one PR (`metal-stack lab libvirt image`) without a cooldown, since this repository's own `main` builds the image from a reviewed commit. They are **not** automerged: the image runs privileged as root on the lab's nodes, and the reviewed bump is what the pin is for.

Major updates are **disabled** for all custom-regex managers — these touch deploy-time
CRDs, the OpenStack release matrix, and build tooling where a major bump always needs
human-driven coordination. The `renovate-config-validator` pin is the one exception: it
has to follow the major the hosted bot runs, or the validator silently stops checking
`renovate.json` against the schema that bot enforces. That pin is never automerged
either — the validator run is the only check that exercises the new release, so a bump
must not be able to approve itself into a `test-shell` job that runs on `main` and on
`v*` tag pushes. The OVN pin disables minor updates as well, because the image is held
on the 26.03 LTS line: 26.09 is a minor bump and 27.03 a major one, and both leave that
line. Patch bumps there wait the 3-day cooldown but are **not** automerged, for the
reason below. `build-images.yaml` runs on every `pull_request` touching `images/**`,
builds the amd64 image and runs `tests/container-images/verify_ovn.sh` against it, so
the reviewer merges on a green build.

Go module PRs run two post-update options. `gomodTidy` runs `go mod tidy` in the
module whose `go.mod` Renovate updates. `gomodTidyAll` then runs it in every module
that points at that module through a local `replace` directive, in dependency order:
every operator module replaces `internal/common`, and `operators/c5c3` also replaces
the service operators. A bump in `internal/common` that raises an indirect requirement
of the operators therefore arrives tidy in every module. Both options stay listed
because the validator does not check `postUpdateOptions` values: a hosted bot too old
for `gomodTidyAll` would silently skip it, and `gomodTidy` still runs. `go.work.sum` is
not tracked (see `.gitignore`). The go command appends a checksum to it whenever a
workspace build needs one that no member `go.sum` holds, so its content depends on
which command ran, and Renovate's gomod manager writes it only when it vendors.

Separately, the native `nix` manager keeps the development flake fresh: it maintains
`flake.lock` (the pinned `nixpkgs` revision) via lock-file maintenance, opening a grouped
weekly re-lock PR. That PR is **not** automerged: `nixos-unstable` is a rolling ref, so the
re-lock is not a version bump and the 3-day cooldown cannot gate it — a reviewer confirms
the base-runtime bump instead. The Nix devshell re-reads the canonical tool pins
(`ci.yaml`, the `Makefile`, `hack/install-test-deps.sh`) on entry, so a tool-version
bump needs **no** flake edit — see [Nix Development Environment](./nix-dev-environment.md).

### Flux HelmRelease chart versions

Renovate's native `flux` manager reads the Flux manifests through the two
`flux.managerFilePatterns` entries in `renovate.json`:

- `/^deploy/flux-system/(releases|sources)/.+\.yaml$/`
- `/^deploy/kind/.+\.yaml$/`

The manager resolves the `sourceRef` of a HelmRelease against the HelmRepository
manifests it has parsed, so the sources directory has to match as well:
`deploy/kind/prometheus/release.yaml` references the `prometheus-community`
HelmRepository in `deploy/flux-system/sources/`. Nothing under `deploy/lab/` or
`deploy/examples/` matches. The patterns use no regex lookahead, because the hosted bot
compiles them with RE2, which has none.

It tracks sixteen third-party charts and one image:

| Chart | File | Strategy | Automerge |
| --- | --- | --- | --- |
| `cert-manager` | `deploy/flux-system/releases/cert-manager.yaml` | `widen` | no |
| `external-secrets` | `deploy/flux-system/releases/external-secrets.yaml` | `widen` | no |
| `garage-operator` | `deploy/flux-system/releases/garage-operator.yaml` | `widen` | no |
| `mariadb-operator` | `deploy/flux-system/releases/mariadb-operator.yaml` | `widen`, group `mariadb-operator chart` | no |
| `mariadb-operator-crds` | `deploy/flux-system/releases/mariadb-operator-crds.yaml` | `widen`, group `mariadb-operator chart` | no |
| `openbao` | `deploy/flux-system/releases/openbao.yaml` | `widen` | no |
| `prometheus-operator-crds` | `deploy/flux-system/releases/prometheus-operator-crds.yaml` | `widen`, group `prometheus-operator charts` | no |
| `chaos-mesh` | `deploy/kind/chaos-mesh/release.yaml` | `widen` | no |
| `grafana` | `deploy/kind/dizzy/release-grafana.yaml` | `widen` | no |
| `victoria-metrics-single` | `deploy/kind/dizzy/release-victoria-metrics.yaml` | `widen` | no |
| `metrics-server` | `deploy/kind/metrics-server/release.yaml` | `widen` | no |
| `kube-prometheus-stack` | `deploy/kind/prometheus/release.yaml` | `widen`, group `prometheus-operator charts` | no |
| `vertical-pod-autoscaler` | `deploy/kind/vpa/release.yaml` | `widen` | no |
| `gateway-helm` | `deploy/kind/base/envoy-gateway.yaml` | `bump`, group `envoy-gateway` | minor/patch after 3 days |
| `headlamp` | `deploy/kind/base/headlamp.yaml` | `bump`, group `headlamp` | minor/patch after 3 days |
| `csi-driver-nfs` | `deploy/kind/nfs/release.yaml` | exact pin, majors disabled, group `csi-driver-nfs chart` | no |
| image `ghcr.io/headlamp-k8s/headlamp-plugin-flux` | `deploy/kind/base/headlamp.yaml` (HelmRelease values) | tag | no |

`widen` is the `rangeStrategy` of the base rule. A release inside the range changes
nothing in Git: Flux installs it on its next reconcile and Renovate stays silent. A
release past the ceiling opens a PR that raises the ceiling and keeps the floor, so
`>=0.10.0 <1.0.0` becomes `>=0.10.0 <3.0.0` for `external-secrets` 2.x. Renovate's
default strategy resolves to `replace` here, which writes a bare `<3.0.0` and drops the
floor. `widen` never moves the floor. `gateway-helm` and `headlamp` use `bump` for that:
an in-range release rewrites the floor (`>=1.9.0 <2.0.0` becomes `>=1.9.2 <2.0.0`), and
a major rewrites both bounds (`>=2.0.0 <3.0.0`).

Two more rules make the manager work on the sources of this repository:

- A chart from a `type: oci` HelmRepository (`c5c3-charts`, `prometheus-community`,
  `garage-operator`, `envoy-gateway-charts`) resolves through the `docker` datasource,
  whose `docker` versioning cannot read a range. Renovate then skips the chart with
  `skipReason: invalid-value`. A packageRule sets `versioning: helm` for a `docker`
  dependency of the flux manager whose current value starts with `>=`. The rule names
  no chart, so it covers a new OCI chart as long as its range is spelled `>=X <Y`; a
  `^X` or `~X` range is still skipped.
- The base rule sets `minimumReleaseAgeBehaviour: timestamp-optional`. `ghcr.io`
  publishes no release timestamp, and under the default, `timestamp-required`, the
  3-day cooldown holds every `ghcr.io` update as pending forever. Docker Hub and the
  HTTP chart indexes publish timestamps, so their cooldown still holds. None of the
  `ghcr.io` dependencies automerges.

Two rules switch the manager off. The `c5c3-charts` releases match
`ghcr.io/c5c3/charts/**`, the `url` of `deploy/flux-system/sources/c5c3-charts.yaml`
without `oci://`: this repository publishes those charts and sets their ranges itself.
The other rule lists every file a customManager reads, plus the K-ORC release, whose
image stays untracked and moves with the `bump-korc-pin` skill:

- `deploy/flux-system/sources/k-orc.yaml`
- `deploy/flux-system/sources/openbao-operator.yaml`
- `deploy/flux-system/sources/rabbitmq-cluster-operator.yaml`
- `deploy/flux-system/releases/k-orc.yaml`
- `deploy/flux-system/releases/rabbitmq-cluster-operator.yaml`
- `deploy/kind/base/flux-web.yaml`
- `deploy/kind/infrastructure/openbao-instance.yaml`
- `deploy/kind/nfs/nfs-server.yaml`

Every pin has one owner. A customManager moves a tag together with its digest or commit
in one match, which the flux manager does not do for these pairs, so a file that gets a
customManager joins this list. `tests/unit/renovate/flux_helmrelease_manager_test.sh`
fails for a file both managers read, and R8 of the `check-renovate-coverage` audit
reports it as claimed twice.

A `gateway-helm` or `headlamp` major opens a PR that is not automerged. The
`ENVOY_GATEWAY_VERSION` CRD pin shares the `envoy-gateway` group with the chart, but its
majors stay disabled like every custom-regex major. The chart comes from Docker Hub and
the pin from GitHub releases, so the group holds both only once both releases are
visible and past the cooldown. The pin can automerge alone while it stays inside the
chart range. A chart PR whose new range leaves the pin behind (a floor bump that
arrives first, or a major) fails `tests/unit/renovate/envoy_gateway_manager_test.sh`
until the pin is inside the range.

A PR that raises a ceiling lets Flux install every release up to the new one. Before
merging, the reviewer handles the coupling the e2e legs do not see:

- `victoria-metrics-single` (`deploy/kind/dizzy/release-victoria-metrics.yaml`) and
  `vertical-pod-autoscaler` (`deploy/kind/vpa/release.yaml`) hold a one-minor window on
  purpose. `widen` keeps the old floor, so lift the floor by hand on the PR branch and
  keep the window one minor wide.
- A `gateway-helm` major needs `ENVOY_GATEWAY_VERSION` in `hack/deploy-infra.sh` moved by
  hand on the same branch; the test above stays red until it is.
- A `mariadb-operator` major moves the `github.com/mariadb-operator/mariadb-operator` Go
  module (pinned in `internal/common/go.mod` and the operator modules) along with the
  CRDs the chart installs.
- `prometheus-operator-crds` owns the CRDs `kube-prometheus-stack` runs against
  (`deploy/kind/prometheus/release.yaml` sets `crds.enabled=false`), so the two share the
  `prometheus-operator charts` group. A PR that raises only the `kube-prometheus-stack`
  ceiling needs a check that the CRDs below the `prometheus-operator-crds` ceiling carry
  what the new operator expects.

To see what the manager proposes, run Renovate against a copy of `deploy/`. The npx
install of Renovate lacks the optional `re2` module and then rejects every
`matchStrings` entry that uses `(?m)`. The flux manager needs no customManager, so the
copy drops them:

```bash
tmp=$(mktemp -d) && cp -R deploy "$tmp/deploy"
jq 'del(.customManagers) | .enabledManagers = ["flux"]' renovate.json > "$tmp/renovate.json"
git -C "$tmp" init -q && git -C "$tmp" add -A && git -C "$tmp" -c user.name=x -c user.email=x@x commit -qm x
ver=$(sed -nE 's/^RENOVATE_VALIDATOR_VERSION="([^"]+)"/\1/p' tests/unit/renovate/fluxoperator_custommanager_test.sh)
(cd "$tmp" && npx --yes --package node@24 --package "renovate@${ver}" -- \
  renovate --platform=local --report-type=file --report-path="$tmp/report.json")
jq -r '.repositories[].packageFiles.flux[] | .packageFile as $f | .deps[]
  | [$f, .depName, (.currentValue // .currentDigest), (.skipReason // "tracked"),
     ((.updates // []) | map(.updateType + " " + (.newValue // .newDigest)) | join(" | "))] | @tsv' "$tmp/report.json"
```

`$ver` is the `RENOVATE_VALIDATOR_VERSION` that
`tests/unit/renovate/fluxoperator_custommanager_test.sh` pins; that release refuses
Node 25, hence `--package node@24`. Each row names the file, the dependency, its
current value, `tracked` or the skip reason, and any proposed update. The run has two limits.
`--platform=local` stops after the lookup and builds no branches, so the rules scoped
by `matchUpdateTypes` (the `csi-driver-nfs` major, the two automerge rules) do not show
in the report; `flux_helmrelease_manager_test.sh` covers them. And `bump` proposes
nothing while a floor equals the newest release: lower the floor in the copy, for
example to `>=1.9.0 <2.0.0` in `deploy/kind/base/envoy-gateway.yaml`, to see
`>=1.9.2 <2.0.0` proposed for the `renovate/envoy-gateway` branch.

### Pins with no datasource: the OVN image content pins

`images/ovn/Dockerfile` carries two pins Renovate deliberately does **not** track:

| Pin | What it names |
| --- | --- |
| `ARG OVN_COMMIT` | The commit `ARG OVN_VERSION` resolves to today, and the ref the build fetches |
| `ARG OVS_COMMIT` | The commit the `ovs` submodule gitlink names at that commit |

Both are content pins, standing to `OVN_VERSION` as the `sha256` digest stands to
`ubuntu:noble`, and no Renovate datasource can express "the commit at tag X" or "the
`ovs` gitlink at tag X". So a tag bump moves `OVN_VERSION` and leaves both behind. The
build fetches `OVN_COMMIT` rather than the tag, so it still compiles the old release and
CI fails one step later, in `tests/container-images/verify_ovn.sh`: the version the
binaries report no longer matches the bumped pin. **Owner: the reviewer of the Renovate
PR** — they clone the new tag, read both SHAs with
`git -C <ovn-clone-at-tag> rev-parse HEAD` and `rev-parse HEAD:ovs`, and commit them
onto the branch. That step is why the OVN patch rule does not automerge: an automerged
PR would leave nobody to carry the pins across, and the group would stall red until
someone noticed OVN security patches had stopped landing.

`ARG NOVNC_COMMIT` in `images/nova/Dockerfile` is the exception to that split.
Renovate tracks it as the `currentDigest` of the noVNC manager: a plain tag
has no second gitlink to carry across, and the github-tags datasource resolves
the tag to the commit it names.

### Re-cutting the openstack-hypervisor-operator patch

The four patches under `images/openstack-hypervisor-operator/patches/` are cut
against the pinned upstream commit, each as one commit on top of the ones before:

| Patch | Subject | Test command |
| --- | --- | --- |
| `0001-eviction-let-nova-choose-block-migration.patch` | `Eviction: let Nova choose block migration` | `go test -count=1 -run '^TestLiveMigrateAutoBody$' ./internal/controller/eviction/` |
| `0002-hypervisor-make-the-high-availability-default-configurable.patch` | `Hypervisor: make the default of spec.highAvailability configurable` | `go test -count=1 -run '^TestHypervisorCreatedWithDefaultHighAvailability$' ./internal/controller/` |
| `0003-traits-report-traitsupdated-when-nothing-differs.patch` | `Traits: report TraitsUpdated when no custom trait differs` | `go test -count=1 -run '^TestTraitsInSyncSetsTraitsUpdated$' ./internal/controller/` |
| `0004-openstack-select-the-catalog-interface-with-os-interface.patch` | `openstack: select the catalog interface with OS_INTERFACE` | `go test -count=1 -run '^TestServiceClientInterface$' ./internal/openstack/` |

When a Renovate PR moves `ARG HVO_COMMIT` to a commit on which one of them no
longer applies, `build-hvo` fails at the `git apply` step and nothing is
published. If upstream now carries a patch's change, delete that patch on the
Renovate branch together with its checks in the Dockerfile's build step: for 0001
the `servers.LiveMigrateOpts` grep and the `TestLiveMigrateAutoBody` run, for
0002 and 0003 the test's name in the controller `go test` run and the `grep` for
its `--- PASS:` line, for 0004 the `TestServiceClientInterface` run and its
`grep`. Each test passes only while upstream ships it. With no patch
left, the `COPY patches/` and `git apply` steps fail as well, so drop them in the
same change. Otherwise re-cut the patches in order in a scratch clone that holds
both the old and the new commit, so a three-way apply finds the blobs each patch
was cut from:

```bash
old=<commit before the move>; new=<commit the Renovate PR pins>
git init /tmp/hvo && cd /tmp/hvo
git remote add origin https://github.com/cobaltcore-dev/openstack-hypervisor-operator.git
git fetch --depth 1 origin "$old" "$new" && git checkout --detach "$new"
git apply --3way <repo>/images/openstack-hypervisor-operator/patches/0001-*.patch
# resolve the conflicts, then prove the test still passes
go test -count=1 -run '^TestLiveMigrateAutoBody$' ./internal/controller/eviction/
git commit -am "Eviction: let Nova choose block migration"
# the same for 0002, 0003 and 0004, each with its test command and subject from the table
git format-patch -4 --no-signature -o /tmp/hvo-patches
```

`git am -3` does not take the checked-in files: each carries the house header instead
of a mail envelope, and `git am` stops at the missing author. Turn each
`git format-patch` output back into the house header (the SPDX pair, the bare subject,
the rationale, `Applies-to:` naming the new commit, `Upstream status:`, and no
diffstat; see `patches/cinder/2025.2/0001-nfs-run-qemu-img-info-as-the-service-user.patch`
for the form), commit the files onto the Renovate branch and let `build-hvo` prove
them.

### Re-cutting the kvm-node-agent patch

`images/kvm-node-agent/patches/0001-certificates-restrict-private-key-file-modes.patch`
is cut against the pinned upstream commit, and the recipe is the one above with the
kna names. When a Renovate PR moves `ARG KNA_COMMIT` to a commit on which the patch
no longer applies, `build-kna` fails at the `git apply` step and nothing is
published. If upstream now carries the change, delete the patch on the Renovate
branch together with its two test runs in the Dockerfile's build step, and with the
last patch the `COPY patches/` and `git apply` steps. Otherwise re-cut it in a
scratch clone that holds both commits:

```bash
old=<commit before the move>; new=<commit the Renovate PR pins>
git init /tmp/kna && cd /tmp/kna
git remote add origin https://github.com/cobaltcore-dev/kvm-node-agent.git
git fetch --depth 1 origin "$old" "$new" && git checkout --detach "$new"
git apply --3way <repo>/images/kvm-node-agent/patches/0001-*.patch
# resolve the conflicts, then prove both tests still pass
go test -count=1 -run '^TestUpdateTLSCertificateKeyMode$' ./internal/certificates/
go test -count=1 -run '^TestUpdateTLSCertificateKeyGroupNotPermitted$' ./internal/certificates/
git commit -am "Certificates: restrict the mode of private key files"
git format-patch -1 --no-signature --stdout > /tmp/0001.patch
```

Run the second test as a user other than root: it skips as root, because a root
process may give a file any group. The image build runs the first test as root and
the second as uid 65534, and fails unless both print their `--- PASS:` line. Turn
the `git format-patch` output into the house header as for the hvo patch (the SPDX
pair, the bare subject, the rationale, `Applies-to:` naming the new commit,
`Upstream status:`, and no diffstat), move the chart ref in
`deploy/lab/metal-stack/hypervisor/sources.yaml` to the chart of the new commit,
commit both onto the Renovate branch and let `build-kna` prove the patch.

---

## Go version upgrades

### Cadence and support window

The Go team ships a new minor release roughly every six months and supports the
**two most recent minor versions**. We track upstream:

- Stay on a supported minor at all times.
- Pick up patch releases (`1.X.Y` → `1.X.(Y+1)`) within the regular Renovate
  flow — these are low-risk and grouped with other patch bumps.
- Plan minor upgrades (`1.X` → `1.(X+1)`) within roughly one month of GA so
  the deprecation window before the *previous* minor falls out of support is
  comfortable.

### Where the Go version lives

A minor upgrade must touch **every** location in the same PR — partial bumps
fail CI because `go.work` and the per-module `go.mod` directives must agree.

| File                               | What to update                       | Notes                                                                        |
| ---------------------------------- | ------------------------------------ | ---------------------------------------------------------------------------- |
| `go.work`                          | `go 1.X.Y`                           | Single source of truth used by `actions/setup-go` in CI.                     |
| `internal/common/go.mod`           | `go 1.X.Y`                           | Shared library module.                                                       |
| `operators/keystone/go.mod`        | `go 1.X.Y`                           | Keystone operator module.                                                    |
| `operators/c5c3/go.mod`            | `go 1.X.Y`                           | C5C3 ControlPlane orchestrator module.                                       |
| `operators/Dockerfile`             | `FROM golang:1.X@sha256:…`           | Renovate maintains the floating tag and digest; the single parameterized Dockerfile builds every operator. |
| `.github/workflows/ci.yaml`        | *No change.*                         | All `actions/setup-go` steps use `go-version-file: go.work`.                 |

There is **no `toolchain` directive** anywhere in the workspace. We rely on
the directive in `go.mod`/`go.work` plus the toolchain that ships with the CI
runner and the builder image. If the team ever needs to support contributors
on an older local Go, add a `toolchain go1.X.Y` line to `go.work` rather than
to individual `go.mod` files.

### Worked example: 1.25.10 → 1.26.3

An illustrative past minor upgrade (the workspace has since moved on — at the time of
writing all four files are on `go 1.27.1`). Substitute the current and target versions
for your own bump; the mechanics below are unchanged.

```bash
# 1. Bump the version in all four files. Each file has exactly one
#    `go 1.25.10` line; replace with `go 1.26.3`.
sed -i.bak 's/^go 1\.25\.10$/go 1.26.3/' \
  go.work \
  internal/common/go.mod \
  operators/keystone/go.mod \
  operators/c5c3/go.mod
rm -f go.work.bak internal/common/go.mod.bak operators/*/go.mod.bak

# 2. Resync the workspace. This refreshes the indirect requirement lists
#    in each `go.mod`.
go work sync

# 3. Build every module from its own directory (the workspace cannot be
#    built from the repository root because the modules are independent).
(cd internal/common         && go build ./... && go vet ./...)
(cd operators/keystone      && go build ./... && go vet ./...)
(cd operators/c5c3          && go build ./... && go vet ./...)

# 4. Run unit tests for all three modules — each has its own suite.
(cd internal/common    && go test -short -timeout 5m ./...)
(cd operators/keystone && go test -short -timeout 5m ./...)
(cd operators/c5c3     && go test -short -timeout 5m ./...)
```

The shared operator Dockerfile (`operators/Dockerfile`, parameterized via
`--build-arg OPERATOR=<op>`) carries the `FROM golang:1.27@sha256:…` builder line,
maintained by Renovate — confirm that the digest update has landed on `main` *before*
opening the minor-bump PR, otherwise the builder image lags behind the `go.mod`
directive and CI fails on the image-build job.

Commit messages follow the repository convention (English, no Co-Authored-By,
SAP AI-assisted / On-behalf-of / Signed-off-by trailers).

### Verification

Required CI checks gate the upgrade:

- `lint` — `make lint` via `golangci-lint`. New analyzers may light up after
  a minor bump; fix in the same PR or temporarily disable the offending
  linter in `.golangci.yml` with a TODO referencing the upgrade PR.
- `format-check` — `gofumpt -l` must be silent.
- `test` — per-module `go test ./...`.
- `govulncheck` — runs against the new toolchain.
- `e2e-operator (keystone)` / `e2e-operator (c5c3)`, `e2e-infra`, `e2e-prometheus`,
  `e2e-chaos` — Chainsaw suites against a kind cluster (e2e runs as a per-operator
  matrix); these catch runtime regressions that pure Go-level tests miss.

For local pre-flight, the minimal smoke is `go work sync && (cd internal/common && go test -short -timeout 5m ./...)`.

### Deprecated APIs and new vet checks

Each Go minor release adds analyzers and may flip previously-warned APIs to
errors. After `go vet ./...` runs clean from each module, check the release
notes for:

- **New `vet` analyzers.** A minor bump may add an analyzer that triggers on
  patterns the codebase has tolerated for years. If the warning is
  load-bearing, fix it; if it is noise on the path the project has chosen,
  add a targeted exclusion in `.golangci.yml`.
- **`stdlib` deprecations.** Search for newly-deprecated stdlib symbols
  (`grep -rn 'pkg.OldFunc\|pkg.OldType' --include='*.go'`) and replace them.
  Avoid making this part of the upgrade PR if the replacement is invasive —
  open a follow-up.
- **Toolchain selection changes.** `go.work` and `go.mod` only set the
  *minimum* required Go. They do not pin the toolchain that builds the
  binary. Production builds use the Dockerfile `FROM golang:1.X` tag.

### Rollback

If a minor upgrade lands on `main` and a regression surfaces in CI or
production:

1. Revert the single upgrade commit (`git revert <sha>`); the revert is
   self-contained because all four files moved together.
2. If the regression is from a *new vet analyzer*, prefer adding the
   analyzer exclusion to `.golangci.yml` and rolling forward — revert is
   cheap, but the deprecation window of the previous minor will eventually
   force the upgrade.
3. Record the regression in the upgrade issue with a reproducer; do not
   re-attempt the minor bump until the regression has an upstream fix or a
   local workaround.

---

## Library and dependency updates

### Classification

| Type  | Renovate behaviour                                      | Reviewer depth                                                            |
| ----- | ------------------------------------------------------- | ------------------------------------------------------------------------- |
| Patch | Grouped into a PR; **not** auto-merged — a human merges. | Skim the diff for unexpected indirect bumps, then merge once green.       |
| Minor | Grouped into a PR; **not** auto-merged.                  | Confirm the CHANGELOG mentions no behavior change in modules we depend on, then merge. |
| Major | Always opened, never auto-merged.                       | Read full upstream release notes; check for migrations; run e2e locally.  |

This is the behaviour for the **native** managers (Go modules, Docker, Actions, npm,
Flux). The **custom-regex** managers (OpenStack tags, the `hack/` test tooling, kind
deploy components, Go build tooling) and the two floor-tracked Flux charts,
`gateway-helm` and `headlamp`, are the only ones that *do* auto-merge: patch/minor
auto-merge after the 3-day cooldown. Majors are disabled for the custom-regex
managers; a `gateway-helm` or `headlamp` major opens a PR that a human merges.

### Coupled stacks (k8s)

Kubernetes Go packages are versioned in lockstep. **Never** bump just one of:

- `k8s.io/api`
- `k8s.io/apimachinery`
- `k8s.io/client-go`
- `k8s.io/apiextensions-apiserver`
- `k8s.io/component-base`

…without bumping the others to the **same** `v0.X.Y`. Renovate groups these
under a single PR, but if you cherry-pick from a Renovate PR or open a
manual bump, keep them aligned. The same applies to `sigs.k8s.io/controller-runtime`
— it has a hard compatibility matrix with the `k8s.io/*` packages; consult the
[controller-runtime release notes](https://github.com/kubernetes-sigs/controller-runtime/releases)
for the supported pairing before bumping either side.

### OpenBao / Vault clients

`github.com/openbao/openbao/api` is API-compatible with `github.com/hashicorp/vault/api`
but the modules version independently. When OpenBao publishes a new client
release:

- Patch/minor: handle via Renovate.
- Major: open a dedicated PR that also re-runs `tests/e2e/keystone/openbao-*`
  Chainsaw suites and reviews `internal/common/bootstrap/` for any
  signature changes.

### Kubebuilder / operator-sdk markers

Marker generation (`controller-gen`) is pinned via `CONTROLLER_GEN_VERSION`
in `.github/workflows/ci.yaml`. A bump to `controller-gen` may change CRD
generation output (e.g. new schema fields for `+kubebuilder:validation:*`
markers). Treat this as a major-style review:

1. Bump the env var in `ci.yaml`.
2. Run `make manifests` locally and inspect the diff in
   `operators/keystone/config/crd/`.
3. Confirm no CRD field is silently removed (would break running clusters
   on upgrade).

### Docker base image digests

Renovate maintains both the floating tag and the SHA digest, e.g.

```dockerfile
FROM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS builder
```

The pattern (recently exercised in #330 and #342): the floating tag is
human-readable and survives across minor bumps; the digest is the
verifiable, immutable pin that production builds resolve. Both move
together in Renovate PRs. Manual edits should preserve this shape — never
drop the digest.

Distroless and other runtime base images follow the same convention:

```dockerfile
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
```

### GitHub Actions pinned by SHA

Policy: **every** `uses:` reference pins to a commit SHA, annotated with the
human-readable tag for review:

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
- uses: actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c # v6
```

The tag in the trailing comment is for humans. The SHA is what GitHub
resolves. Renovate keeps both in sync. Never replace a SHA with a
floating tag — supply-chain integrity for third-party actions depends on
the SHA pin.

### Security updates (CVEs)

CVE-driven bumps bypass the 3-day Renovate cooldown — merge as soon as CI
is green. Triage order:

1. **CVEs in our direct dependencies** (anything listed in a `go.mod`'s
   direct `require ( … )` block) — top priority. Renovate flags these with
   a high-priority label.
2. **CVEs in indirect dependencies** that affect us — confirm via
   `govulncheck` (run automatically in CI) before deciding urgency.
3. **CVEs in base images** — bump the image digest and re-trigger the
   image-build pipeline.

If `govulncheck` reports a vulnerable indirect dependency that has no
upstream fix yet, document it in the corresponding issue and add a
`replace` directive in `go.mod` only as a last resort.

---

## Worked example: a library major upgrade

Major bumps always need human review. Take a hypothetical
`controller-runtime` v0.23 → v1.0 PR:

1. **Read the upstream release notes** in full. controller-runtime
   typically gates Kubernetes API compatibility on its minor versions —
   confirm the supported k8s.io pairing matches the version we ship.
2. **Confirm the API surface we touch.** Search the codebase:

   ```bash
   grep -rn 'controller-runtime\|sigs.k8s.io/controller-runtime' \
     internal/common operators
   ```

3. **Run the upgrade locally**, mirroring what Renovate would do:

   ```bash
   (cd operators/keystone && go get sigs.k8s.io/controller-runtime@v1.0.0 && go mod tidy)
   (cd operators/c5c3      && go get sigs.k8s.io/controller-runtime@v1.0.0 && go mod tidy)
   (cd internal/common     && go get sigs.k8s.io/controller-runtime@v1.0.0 && go mod tidy)
   go work sync
   ```

4. **Compile and vet from each module.** Compile errors are the easy
   feedback; for behavior changes, read the controller-runtime CHANGELOG
   for the entries between the two versions.
5. **Run unit tests and the keystone e2e suite locally** (`make test`,
   `make e2e`) before pushing — major operator-framework bumps regularly
   surface only at reconcile-time, not at compile-time.
6. **Open a dedicated PR**, not a Renovate edit. Link the upstream
   release notes, the list of API surfaces touched, and the e2e run
   evidence.

---

## Process

### Renovate PRs vs. dedicated PRs

| Situation                                                                 | Channel                |
| ------------------------------------------------------------------------- | ---------------------- |
| Routine patch/minor bump (Go module, Docker digest, GitHub Action).       | Renovate PR.           |
| Major bump for a leaf dependency with a small API surface.                | Renovate PR + review.  |
| Major bump for `controller-runtime`, `client-go`, `k8s.io/*`, OpenBao.    | **Dedicated PR**.      |
| Go minor upgrade.                                                         | **Dedicated PR + issue**. |
| CVE-driven bump.                                                          | Renovate PR, fast-merge. |
| Anything that requires CRD or Helm chart migration.                       | **Dedicated PR + issue**. |

A dedicated PR is preferred whenever the change requires a write-up that
does not fit in a Renovate commit message: migrations, deprecation
follow-ups, e2e-suite changes, or a coordinated bump across multiple
modules.

### Required checks before merge

Renovate performs the merge itself (`platformAutomerge` is disabled) and
waits until every check on the PR has succeeded. The checks below are the
ones to watch when merging a dependency PR by hand:

- `lint`, `format-check`, `govulncheck`
- `test` (all three modules)
- `e2e-operator (keystone)` / `e2e-operator (c5c3)`, `e2e-infra` (and `e2e-prometheus`,
  `e2e-chaos` when their path filters trigger)
- `helm-validate` (when chart files change)
- `build-e2e-images` (when image or workflow files change)

Renovate's gate covers every check that runs on the PR. A path filter
can keep a job from starting at all; a manual merge holds itself to the
same bar and treats such a job as unverified, not as passed.

---

## See also

- [`renovate.json`](https://github.com/c5c3/cobaltcore/blob/main/renovate.json) — the
  authoritative Renovate configuration with all custom managers and rules.
- [Renovate documentation](https://docs.renovatebot.com/) — for general
  Renovate concepts referenced in `renovate.json`.
- [Go release notes](https://go.dev/doc/devel/release) — minor-version
  changelog and deprecation announcements.
- [CI Workflow](../reference/ci-cd/ci-workflow.md) — what each required
  check actually runs.
- [Nix Development Environment](./nix-dev-environment.md) — the flake that
  provisions the CI toolchain and how it stays in sync with these pins.
