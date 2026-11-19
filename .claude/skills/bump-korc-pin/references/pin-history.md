# K-ORC pin history and CI symptoms

Reference for [bump-korc-pin](../SKILL.md). Verified against
`git log -- deploy/bases/operators/sources/k-orc.yaml deploy/bases/operators/releases/k-orc.yaml`
and the quay tag API on 2026-09-23.

## Bumps since the main-commit pin

| Commit | Date | Pin | Trigger | go.mod | Range |
|---|---|---|---|---|---|
| 60fd8cd7 | 2026-07-16 | `ef6119d` | first main pin: the c5c3-operator Owns() RoleAssignment | moved (d306d0a1, same day) | — |
| f9dccebb | 2026-08-05 | `c22784c` | quay pruned `commit-ef6119d`; digest 404 | moved; `go work sync` carried `sigs.k8s.io/structured-merge-diff/v6` to the other modules | 10 commits, no `api/` or `config/` file |
| 653b3f12 | 2026-08-31 | `8c8b4d9` | `commit-c22784c` expired 2026-08-31 01:13 UTC | **left on c22784c** | 31 commits, no `config/` or `api/` file |
| d1a68660 | 2026-09-07 | `3f02592` | Region kind merged upstream (k-orc#862) | moved | 26 commits, 5 `api/` + 8 `config/` files (the new kind) |
| c236aeb8 | 2026-09-16 | `ddb5fbb` | move to main HEAD; docs rewritten to the commit-pin form | moved | 31 commits, 7 `api/` + 10 `config/` files: Limit controller, RegionRef on Endpoint and RegisteredLimit, all new fields optional |
| 6a0dc505 | 2026-09-20 | `ae54590` | upstream PR 913: transport errors retryable | moved; docs mirror moved | 9 commits, 22 `api/` files, no `config/` file |
| ca34b418 | 2026-10-14 | `da276e4` | upstream PR 967 evicts the stale catalog; #1202 | moved across the v3 module path (`/v2` to `/v3`) | 98 commits, 35 `api/` + 20 `config/` files: DNSZone and SubnetPool kinds, `Unrestricted` and `Enabled` status fields to `*bool` |

The former pin's tag `commit-da276e4` expired **2026-11-11 16:03:22 UTC**
(quay `expiration`; `end_ts` 1794413002). Every `commit-*` tag expires
28 days after its push (`end_ts - start_ts` = 2419200 s, give or take a
second), so a fresh candidate buys four weeks at most.

## Renovate main-line updates (#1672)

On 2026-11-12 the deployment source moved to main commit
`c48889697d73221e39497dac0ae89e2000d6a0c7`; the controller uses `branch-main`
with digest `sha256:9874744fe80230eb13f76ce4f56fbc318da43164c81ab50b61bb13a75a09a915`.
All four platforms' BuildKit provenance records that source commit. The Go
API remains on `v3.0.0-pre1`; its consumed fields remain compatible.

Renovate now groups source and image digest updates for CI-gated
automerge. The image was previously untracked. Unlike commit tags, the
`branch-main` discovery tag has no expiration, but its old digests can still
be collected after it moves; prompt updates remain necessary.

The range from `da276e44` adds RecordSet and optional DNSZone/RoleAssignment
fields, including system-scoped role assignments. Existing project/domain
assignments remain valid. The deployed c5c3 operator reaches Ready with
these CRDs. Go API updates remain a separate maintenance concern.

## Dead `branch-main` digest, repaired from the commit tag (2026-11-19)

quay drops a `branch-main` digest in the second the tag moves: in the tag
history every old entry's `end_ts` equals the next entry's `start_ts`, and
the manifest answers 404 at once. Upstream main builds several times on a
busy day, so the pin dies with each of them:

| `branch-main` digest | Pushed (UTC) | Dropped (UTC) | Lived |
| --- | --- | --- | --- |
| `sha256:9874744f` | 2026-11-12 11:09:39 | 2026-11-18 20:00:44 | 6 days |
| `sha256:cce03595` | 2026-11-18 20:00:44 | 2026-11-19 11:43:13 | 16 hours |

The Renovate update to `bce09a05` and `sha256:cce03595` opened at 10:24 UTC
on 2026-11-19, passed its checks by 10:57 and merged at 12:08, 25 minutes
after quay had dropped the digest. Every leg that runs
`hack/ci-deploy-korc.sh` then failed on `main` and on the open pull
requests with "Could not read provenance for the pinned K-ORC image".

The repair keeps the source commit and replaces the digest alone with the
index of `commit-bce09a0`,
`sha256:bf8553c502998e6051d3d6b7e3b23e7b69ad7d68d0355e40d50c446087bded07`.
That tag expires on 2026-12-16 20:00:51 UTC. Upstream's
`container_image.yaml` workflow builds and pushes the image twice, once
per tag. Both indexes list the same four platform image manifests and
differ in their four attestation manifests, which is why the digests
differ. All four platforms' provenance of the commit-tag index records
`bce09a05`, and `hack/ci-deploy-korc.sh` brought the controller to
Available from it in a throw-away kind cluster.

Renovate's rules are unchanged, so its next grouped update writes a
`branch-main` digest again, and that digest dies with the upstream build
after it. One upstream build pushed under both tags would give the
`branch-main` digest the commit tag's 28 days.

## Historical commit-pin CI symptoms

**Expired or pruned tag.** The four legs that call `hack/ci-deploy-korc.sh`
go red together, on `main` as well as on every PR that runs them:

- `e2e-operator` (c5c3 leg), step "Install CRDs watched by the
  c5c3-operator";
- `e2e-controlplane`, `e2e-controlplane-sso`, `e2e-external-keystone`, step
  "Deploy K-ORC".

The script's closing `kubectl wait` on the `orc-system` Deployments times
out; the diagnostics show the controller pod in
`ImagePullBackOff` with
`quay.io/orc/openstack-resource-controller@sha256:<digest>: not found`.
Confirm with the status script (Q1 expired, Q2 gone) or directly:

```bash
curl -s 'https://quay.io/api/v1/repository/orc/openstack-resource-controller/tag/?specificTag=commit-<7>' \
  | jq '.tags[] | {name, expiration, manifest_digest}'
crane manifest quay.io/orc/openstack-resource-controller@<digest>   # MANIFEST_UNKNOWN once collected
```

A main run that skipped those legs (path filters) stays green while the pin
is already dead; the next PR that selects them inherits the failure.

**Renovate commit-only bump.** The `renovate/k-orc` branch ("update
https://github.com/k-orc/openstack-resource-controller digest to <7>")
moves `ref.commit` alone. Wherever `hack/ci-deploy-korc.sh` runs, the drift
guard stops it before the clone:

```text
::error::K-ORC image tag commit-<old7> in deploy/bases/operators/releases/k-orc.yaml does not match the pinned commit <new40> in deploy/bases/operators/sources/k-orc.yaml (expected commit-<new7>); bump spec.images[].newTag in lockstep with ref.commit
```

The manifest headers call this the block against automerge. Since 1a0daf83
(2026-08-31) it only fires when a K-ORC leg is selected: a PR that touches
nothing but `deploy/**` is a canary change and runs `e2e-infra` plus the
keystone leg, none of which calls `hack/ci-deploy-korc.sh` (simulated with
`hack/ci-resolve-changes.sh` and `FILTER_e2e_shared=true`: the three
ControlPlane jobs resolve to `false`, `e2e-operators` to keystone only).
The k-orc packageRule sets `automerge: true`, so a Renovate bump can pass
its selected checks with the drift in place; `korc-pin-status.sh` K2 catches
it offline. Answer the PR with the lockstep bump, not a tag-only edit.

**Not a pin problem.** An `Apply failed with 1 conflict` error naming
`kustomize-controller` and `containers[name="manager"].image` in "Deploy
K-ORC" means the Flux `k-orc` Kustomization reconciled before the CI install.
The kind base overlay applies it suspended since #1007
(`tests/unit/deploy/kind_base_korc_suspend_test.sh`); check that patch, not
the pin.

## Local tooling

- `crane`, `jq`, `gh` and `curl` cover every network check; the status script
  degrades to `[INFO] … skipped` without them or offline.
- `controller-gen` and `setup-envtest` often sit in `$(go env GOPATH)/bin`,
  off `PATH`; export it before `make manifests` when an RBAC marker changes
  (a newly owned kind). `make test-integration` resolves `setup-envtest`
  through `GOPATH` itself.
- `hack/ci-deploy-korc.sh` needs no cert-manager and no Flux: a bare
  `kind create cluster` is enough. `GITHUB_TOKEN` is optional; without it the
  clone is anonymous.
