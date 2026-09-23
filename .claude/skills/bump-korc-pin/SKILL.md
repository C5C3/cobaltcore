---
name: bump-korc-pin
description: >-
  Move the K-ORC (OpenStack Resource Controller) upstream main-commit pin in
  lockstep across its four sites — the Flux GitRepository ref.commit, the
  kustomize image newTag commit-<short> plus digest, the reference-docs
  mirror, and the c5c3-operator's Go pseudo-version — after checking the quay
  tag expiry and which upstream api/ and config/ files the move pulls in.
  Use when the e2e-operator (c5c3), e2e-controlplane, e2e-controlplane-sso
  and e2e-external-keystone legs fail together in hack/ci-deploy-korc.sh with
  the orc-system controller pod in ImagePullBackOff
  ("quay.io/orc/openstack-resource-controller@sha256:…: not found"), when a
  Renovate k-orc digest PR moves only ref.commit (hack/ci-deploy-korc.sh then
  fails with "K-ORC image tag … does not match the pinned commit"), when the
  pinned commit-<sha> tag nears its four-week quay expiry, or when the
  c5c3-operator needs a newer upstream K-ORC API.
---

# Bump the K-ORC pin

K-ORC is pinned to an upstream **main commit**, not a release: the
c5c3-operator `Owns()` the `RoleAssignment` and `Region` kinds, and no
release ships them (v2.6.0 is still the latest). Upstream publishes an image
per main commit as `quay.io/orc/openstack-resource-controller:commit-<7 hex>`,
and quay expires every `commit-*` tag **four weeks after the push**
(`branch-*` and `v*` tags do not expire). When the tag lapses quay
garbage-collects the manifest, the pinned digest returns `MANIFEST_UNKNOWN`,
and every job that runs `hack/ci-deploy-korc.sh` times out. The pin is
therefore a recurring chore until the structural fix below lands.

## Where the pin lives

| Site | Field | Moves |
|---|---|---|
| `deploy/flux-system/sources/k-orc.yaml` | `spec.ref.commit` (40 hex) | every bump; the only field Renovate tracks (customManager, datasource `git-refs`, `currentValue` main, automerge after 3 days) |
| `deploy/flux-system/releases/k-orc.yaml` | `spec.images[0].newTag` `commit-<7>` and `digest` | every bump, by hand; the digest resolves the pull, `newTag` is the drift anchor |
| `docs/reference/infrastructure/infrastructure-manifests.md` | K-ORC table, Source and Image rows | every bump since c236aeb8 |
| `operators/c5c3/go.mod` + `go.sum` | pseudo-version `v2.5.1-0.<utc stamp>-<12 hex>` | the documented rule (`docs/contributing/dependency-management.md`) moves it every time; mandatory when upstream `api/` changed |

`hack/ci-deploy-korc.sh` reads the first two at runtime: it requires a
40-hex commit and a `sha256:<64 hex>` digest, asserts
`newTag == commit-<first 7 of ref.commit>` (the drift guard), clones K-ORC at
the commit, and applies `./config/default` with the image pinned by digest.
Four CI jobs call it: `e2e-operator` (c5c3 leg), `e2e-controlplane`,
`e2e-controlplane-sso`, `e2e-external-keystone`.

The one accepted divergence: an expiry-only re-pin whose upstream range
touches no `api/` or `config/` file may leave `go.mod` on the older commit
(653b3f12). The status script classifies a go.mod lag as that case
(`[INFO]`) or as a real drift (`[FAIL]`). History and exact CI symptoms:
[references/pin-history.md](references/pin-history.md).

## Procedure

### 1. Read the current state

```bash
bash .claude/skills/bump-korc-pin/scripts/korc-pin-status.sh
```

- **K1–K4** (offline) — commit shape, drift guard, digest shape, go.mod and
  go.sum, docs mirror. Any `[FAIL]` here fails CI or leaves a stale mirror.
- **Q1** — quay tag of the current pin: `[FAIL]` gone or expired, `[WARN]`
  within `--warn-days` (default 7), and the tag still points at the pinned
  digest.
- **Q2** — the pinned digest resolves (`crane manifest`, else the quay
  manifest API) and the index carries `linux/amd64`.
- **Q3** — the commit is on upstream main; how far main has moved.
- **Q4** — classifies a go.mod lag against the upstream range.

`--offline` skips Q1–Q4; unreachable hosts print `[INFO] … skipped`. Exit
code 1 means at least one `[FAIL]`.

### 2. Pick the candidate

Take the **newest upstream main commit whose quay tag exists**:

```bash
bash .claude/skills/bump-korc-pin/scripts/korc-pin-status.sh --candidate main
gh api 'repos/k-orc/openstack-resource-controller/commits?sha=main&per_page=10' \
  --jq '.[] | .sha[0:7] + " " + .commit.committer.date'
```

Upstream's image workflow pushes the tag after the merge; if main's HEAD has
no tag yet, the candidate check fails with "does not exist on quay" — take
the previous main commit (`--candidate <sha>`). Candidate mode rejects:

- a commit not on main (`compare status 'diverged'`): upstream also pushes
  `commit-*` tags for its `release-2.0` branch, e.g. `commit-67b4d14` on
  2026-09-22;
- a gone or expired tag; an older-than-current candidate is only a `[WARN]`
  (a rollback when every newer tag is gone).

`branch-main` never expires but is re-pointed on every merge, so a digest
resolved from it is garbage-collected on a later push. It is no pin.

### 3. Read the range

Candidate mode lists the changed upstream paths between the current pin and
the candidate (GitHub compare API, capped at 300 files) by top-level
directory and flags every `api/` and `config/` file.

| Range touches | Means | Do |
|---|---|---|
| neither `api/` nor `config/` | CRDs, RBAC, manager and Go API unchanged | move the three manifest sites; move go.mod too unless this is an emergency expiry re-pin |
| `config/` | CRD schemas, RBAC or the manager Deployment changed | read the `config/crd/bases/` diffs of the kinds the c5c3-operator creates (ApplicationCredential, Domain, Endpoint, Project, Region, Role, RoleAssignment, Service, User) |
| `api/` | Go types changed | move go.mod, build and vet c5c3, read the type diffs of those kinds; a new required field or a renamed one breaks the projection |
| upstream `go.mod` / `go.sum` | transitive bumps | run [[check-go-workspace-deps]] after the tidy |

The fake CRDs envtest serves live in
`internal/common/testutil/fake_crds/k-orc/` (spec is
`x-kubernetes-preserve-unknown-fields`, status is typed). Extend them only
when the operator starts owning a new kind or reading a new status field.

### 4. Edit the sites in one commit

Candidate mode ends with the edit set (withheld when the candidate failed a
check). Apply it:

1. `deploy/flux-system/sources/k-orc.yaml` — `commit:` to the full SHA.
2. `deploy/flux-system/releases/k-orc.yaml` — `newTag: commit-<7>` and the
   `digest:` the script printed; `crane digest` on
   `quay.io/orc/openstack-resource-controller:commit-<7>` resolves it too.
3. `docs/reference/infrastructure/infrastructure-manifests.md` — the commit
   in the Source row, the tag in the Image row.
4. Go module:

   ```bash
   cd operators/c5c3
   go get github.com/k-orc/openstack-resource-controller/v2@<12-hex sha>
   go mod tidy
   go build ./... && go vet ./...
   ```

   Commit only the modules the bump changes. When the tidy moves a shared
   dependency (f9dccebb moved `sigs.k8s.io/structured-merge-diff/v6`), bring
   the other workspace modules along per [[check-go-workspace-deps]]; a bare
   `go work sync` has rewritten eight unrelated modules before, so do not
   commit that churn.

Rerun the status script: every K and Q check must pass, and Q1 shows the new
tag's expiry date for the commit message.

### 5. Verify locally

```bash
bash tests/unit/hack/ci_deploy_korc_commit_pin_test.sh
bash tests/unit/renovate/korc_source_custommanager_test.sh
bash tests/unit/deploy/kind_base_korc_suspend_test.sh
make verify-go-tidy
make test-operator OPERATOR=c5c3
```

When `api/` changed, also run the c5c3 envtest suite, about 22 minutes:
`make test-integration OPERATOR=c5c3`.

Deploy the new pin into a throw-away kind cluster, exactly as CI does (about
3–4 minutes; the kind node on Apple silicon pulls the `linux/arm64` image of
the same index):

```bash
kind create cluster --name korc-bump
bash hack/ci-deploy-korc.sh
kubectl get crd -o name | grep openstack.k-orc.cloud
kubectl -n orc-system get deploy,pods
kind delete cluster --name korc-bump
```

The script parses the edited manifests, so a stale `newTag` fails here the
way it fails in CI.

### 6. Commit and open the PR

Subject lines of the previous bumps:

```text
build(k-orc): move the main-commit pin to <7 hex>                  (c236aeb8, 6a0dc505)
fix(deploy): re-pin K-ORC to upstream <7 hex> after its quay tag expired    (653b3f12)
```

The body states why (upstream change or expiry), the sites moved, the range
(commit count, whether `api/` or `config/` changed and what that means for
the kinds the c5c3-operator creates), the new tag's expiry date, and what was
verified. Follow the trailer block of `git show 6a0dc505`.

CI selection matters here:

- A bump that moves `operators/c5c3/go.mod` matches the `c5c3` paths filter,
  which runs all four K-ORC legs.
- A re-pin that touches only the K-ORC manifests (or
  `hack/ci-deploy-korc.sh`) matches the `e2e_korc` filter: the canary
  (`e2e-infra` plus the keystone leg) plus the c5c3 `e2e-operator` leg,
  which runs `hack/ci-deploy-korc.sh` in full. Add `ci:controlplane` to
  also run the three ControlPlane jobs, or `ci:full` for everything.
- Every pull request runs `make test-shell`, and with it
  `tests/unit/deploy/korc_pin_lockstep_test.sh`: the script's offline pin
  gates (`KORC_VERIFY_ONLY=true`) against the committed manifests. It fails
  a commit that moves `ref.commit` without `newTag`, or drops the digest.

A pending Renovate `renovate/k-orc` PR moves only `ref.commit`, so both
gates fail it and its `automerge: true` rule cannot land the drift (until
the lockstep test and the `e2e_korc` filter existed, it selected none of the
four legs and could). Run the status script against its branch, supersede it
with the lockstep PR, and let Renovate autoclose it rather than patching its
branch.

## Structural fix (open)

The four-week clock stays as long as the pin is a main commit. Two ways out:

1. **Return to a release tag** once a K-ORC release ships RoleAssignment and
   Region (`gh release list --repo k-orc/openstack-resource-controller`). The
   headers of both Flux manifests describe the REVERT: `ref.tag` instead of
   `ref.commit`, path `./dist` instead of `./config/default`, drop the
   `images:` override. The Renovate customManager returns from `git-refs` to
   a release datasource, `hack/ci-deploy-korc.sh` loses its commit and
   drift-guard logic (60fd8cd7 introduced it), and go.mod moves to the
   release.
2. **Mirror the image** into a registry without tag expiry and point
   `newName` at it. A byte-for-byte copy of the index keeps its digest. The
   drift guard and this skill's quay checks would then read the mirror.

Neither is decided; record the choice in an issue before starting either.

## Notes

- The status script is read-only; it never edits the manifests. `--candidate`
  prints the edit set, and applying it is the bump itself.
- Pair with [[check-renovate-coverage]] after reverting to a release tag (the
  customManager changes shape) and with [[debug-e2e-failure]] when the four
  legs fail for a reason other than the image pull.
