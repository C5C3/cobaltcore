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

The authoritative configuration lives in [`renovate.json`](https://github.com/c5c3/forge/blob/main/renovate.json)
at the repository root.

---

## Renovate at a glance

Renovate is configured via `config:recommended` plus a small set of custom
regex managers and package rules. The high-level split is:

| What Renovate does on its own                                                    | What needs a human                                            |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| Opens PRs for every dependency it understands (Go, Docker, GitHub Actions, npm). | **Merging** native-manager PRs — Go/Docker/Actions/npm bumps are opened but not auto-merged. |
| Auto-merges only the custom-regex–managed pins (patch/minor) after a 3-day cooldown — see below. | Reviewing major-version bumps.                |
| Maintains Docker image digests (`@sha256:…`) alongside floating tags.            | Reviewing anything touching coupled stacks (k8s).             |
| Pins GitHub Actions to commit SHAs (annotated with a `# vX.Y` tag).              | Updating the Go workspace / `go.mod` directives; triaging CVEs. |

Renovate combines the `config:recommended` native managers (Go modules, Dockerfiles,
GitHub Actions, npm) with **fifteen** custom regex managers that track pins those
native managers cannot see. Auto-merge (patch/minor, 3-day cooldown) is wired **only**
onto the custom-regex managers; native-manager PRs always wait for a human merge
(`config:recommended` does not auto-merge, and no top-level `automerge` is set).

The custom managers cover:

- **OpenStack release refs** — git tags in `releases/*/source-refs.yaml` and PyPI pins in `releases/*/test-refs.yaml`.
- **Test tooling in `hack/`** — Chainsaw, Flux CLI, kind, and kubectl versions in `hack/install-test-deps.sh`, plus `FLUX_OPERATOR_VERSION` in `hack/deploy-infra.sh`.
- **kind deploy components** — `flux-web.yaml`, `envoy-gateway.yaml`, and `headlamp.yaml` under `deploy/kind/base/`.
- **K-ORC Flux source** — the `ref.tag` of the K-ORC `GitRepository` in `deploy/flux-system/sources/k-orc.yaml` (github-releases). This closes a drift gap: without it the Flux-applied K-ORC CRDs could fall behind the Renovate-tracked `k-orc/openstack-resource-controller` Go module the operator compiles against.
- **Go build tooling in `Makefile` / `.github/workflows/*.yaml`** — `gofumpt`, `controller-gen`, `golangci-lint`, and `yq`.

Major updates are **disabled** for all custom-regex managers — these touch deploy-time
CRDs, the OpenStack release matrix, and build tooling where a major bump always needs
human-driven coordination.

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
writing all four files are on `go 1.26.4`). Substitute the current and target versions
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

# 2. Resync the workspace. This refreshes `go.work.sum` and the indirect
#    requirement lists in each `go.mod`.
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
`--build-arg OPERATOR=<op>`) carries the `FROM golang:1.26@sha256:…` builder line,
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

This is the behaviour for the **native** managers (Go modules, Docker, Actions, npm).
The **custom-regex** managers (OpenStack tags, the `hack/` test tooling, kind deploy
components, Go build tooling) are the only ones that *do* auto-merge: patch/minor
auto-merge after the 3-day cooldown, major disabled.

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
FROM golang:1.26@sha256:6df14f4a4bc9d979a3721f488981e0d1b318006377e473ed23d026796f5f4c0a AS builder
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

Renovate auto-merge fires only after the full required-checks set is
green. The set is enforced by branch protection on `main`; the most
load-bearing for dependency PRs are:

- `lint`, `format-check`, `govulncheck`
- `test` (all three modules)
- `e2e-operator (keystone)` / `e2e-operator (c5c3)`, `e2e-infra` (and `e2e-prometheus`,
  `e2e-chaos` when their path filters trigger)
- `helm-validate` (when chart files change)
- `build-e2e-images` (when image or workflow files change)

For a manual merge, wait for the same checks even if Renovate's
auto-merge would have skipped any of them due to path filters — the
filters are an optimization, not an exemption.

---

## See also

- [`renovate.json`](https://github.com/c5c3/forge/blob/main/renovate.json) — the
  authoritative Renovate configuration with all custom managers and rules.
- [Renovate documentation](https://docs.renovatebot.com/) — for general
  Renovate concepts referenced in `renovate.json`.
- [Go release notes](https://go.dev/doc/devel/release) — minor-version
  changelog and deprecation announcements.
- [CI Workflow](../reference/ci-cd/ci-workflow.md) — what each required
  check actually runs.
