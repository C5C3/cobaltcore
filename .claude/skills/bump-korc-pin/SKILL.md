---
name: bump-korc-pin
description: >-
  Diagnose or update the grouped K-ORC main-line source commit and branch-main
  image digest. Verify registry availability, image provenance, API compatibility,
  and Renovate automation without coupling routine image updates to Go/Nix locks.
---

# K-ORC source and image updates

The Flux source pins a full commit from upstream `main`. The controller is
pinned by digest; `branch-main` is its Renovate discovery tag. Renovate owns
both updates in one `k-orc` group, eligible for automerge after required CI.
There is no age delay for a continuously moving main branch.

The Go API dependency remains managed separately. A controller update does
not need the exact same commit in `go.mod` when its consumed API remains
compatible. Do not couple every source/image refresh to a Go bump or Nix
vendor-hash regeneration: that would require recurring manual work and block
the image-update automation. Actual API incompatibility must fail CI and be
addressed as a code/dependency change.

Upstream expires `commit-*` image tags after 28 days. `branch-main` has no
configured expiration, but moves with each build, so old digests may still
be collected. Renovate updates must keep merging. A digest provides
reproducibility while retained, not permanent artifact storage.

## Diagnose a blocked update

1. Inspect the grouped Renovate PR and current pins:

   ```bash
   bash .claude/skills/bump-korc-pin/scripts/korc-pin-status.sh
   ```

   `--offline` checks local fields and docs. Online checks report retention
   and digest availability. The Go API version is reported separately.

2. Check the source and image references:

   ```bash
   gh api repos/k-orc/openstack-resource-controller/commits/main --jq .sha
   docker buildx imagetools inspect quay.io/orc/openstack-resource-controller:branch-main
   ```

   Main can advance before its image finishes publishing. The installer
   inspects the **pinned digest's** BuildKit provenance and requires every
   platform's VCS revision to match the source commit before any Kubernetes
   write. A partial update stays red until both pins have caught up.

3. If a one-time repair is needed, update the two immutable pins together:

   - `deploy/bases/operators/sources/k-orc.yaml`: full `ref.commit`.
   - `deploy/bases/operators/releases/k-orc.yaml`: `newTag: branch-main` and
     `digest: sha256:<64 hex>`.

   Read upstream changes under `api/` and `config/`, especially the Keystone
   kinds c5c3 consumes. Existing optional-field additions do not automatically
   require updating the Go API module. If consumed APIs change, follow
   [[check-go-workspace-deps]] and the repository's Nix-lock workflow in that
   explicitly scoped compatibility fix.

4. Verify:

   ```bash
   bash tests/unit/hack/ci_deploy_korc_commit_pin_test.sh
   bash tests/unit/hack/ci_deploy_korc_main_image_test.sh
   bash tests/unit/deploy/korc_pin_lockstep_test.sh
   bash tests/unit/deploy/korc_flux_source_test.sh
   bash tests/unit/deploy/overlays/kind_base_korc_suspend_test.sh
   bash tests/unit/renovate/korc_source_custommanager_test.sh
   make test-operator OPERATOR=c5c3
   ```

   Follow [[check-renovate-coverage]] for update-rule changes. Use a disposable
   kind cluster and isolated kubeconfig for `hack/ci-deploy-korc.sh`. It needs
   Docker Buildx and jq for the provenance check; the CI jobs provision Buildx.
   Confirm the controller is Available and watches every namespace. Verify
   the Flux bootstrap path for manifest changes, then remove test clusters.
   Run integration tests when changing the consumed API or Go dependency.

5. Record the source, image digest, compatibility review and tests in the PR
   and [pin-history.md](references/pin-history.md). Reference docs link to the
   manifest instead of duplicating a commit Renovate would leave stale.
   Follow the repository's fork and commit rules. Routine future updates
   belong to Renovate, not a manual pin-maintenance process.
