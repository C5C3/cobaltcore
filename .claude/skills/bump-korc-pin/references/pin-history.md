# K-ORC pin history and CI symptoms

Reference for [bump-korc-pin](../SKILL.md). Verified against
`git log -- deploy/flux-system/sources/k-orc.yaml deploy/flux-system/releases/k-orc.yaml`
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

The current pin's tag `commit-ae54590` expires **2026-10-15 14:11:56 UTC**
(quay `expiration`; `end_ts` 1792073516). Every `commit-*` tag expires
28 days after its push (`end_ts - start_ts` = 2419200 s, give or take a
second), so a fresh candidate buys four weeks at most.

## CI symptoms

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
::error::K-ORC image tag commit-<old7> in deploy/flux-system/releases/k-orc.yaml does not match the pinned commit <new40> in deploy/flux-system/sources/k-orc.yaml (expected commit-<new7>); bump spec.images[].newTag in lockstep with ref.commit
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
