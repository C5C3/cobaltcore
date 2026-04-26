# Review Pattern: Verify cross-file consistency across the PR scope

**Review-Area**: documentation
**Detection-Hint**: Whenever a PR plan promises a change that should be reflected in more than one place — an identifier rename, a doc-file update, a submodule pointer bump, a generated reference file, a CHANGELOG entry — grep the full PR diff for every place the change should land. Common misses: markdown docs, tables, comments, string literals, submodule pointers (`git diff main...HEAD -- <submodule-path>` is empty), and external references the PR description claims to update.
**Severity**: WARNING
**Occurrences**: 2

## What to check

For each cross-file commitment a PR makes — identifier renames, documentation updates, submodule pointer bumps, external-doc references — verify the corresponding artifact actually changed in the diff. For renames: grep the full changeset for the old name. For doc/architecture updates promised in the PR description: confirm either the file changed in the diff, the submodule pointer was bumped, or the commit log links to the external commit/PR that captures the change.

## Why it matters

Stale references in documentation mislead readers and erode trust in docs accuracy. When a rename is done to address one review comment but docs aren't updated, it signals the change was applied mechanically without checking for ripple effects.

## Examples from external reviews

### CC-0012 — greptile-apps[bot]
- **Feedback**: The table entry references `TestIntegration_CELRejectsReplicasZero`, but this test was renamed to `TestIntegration_CELRejectsReplicasBelowMinimum` in `operators/keystone/api/v1alpha1/integration_test.go` as part of this very PR. The documentation wasn't updated to match.
- **What was missed**: After any identifier rename in a PR, grep the full changeset for the old name to ensure no stale references remain — especially in markdown docs, tables, comments, and generated reference files.
- **Fix**: Updated the documentation table in `docs/reference/keystone-crd.md` to use the new test name `TestIntegration_CELRejectsReplicasBelowMinimum` and adjusted the description to reflect the actual test behavior (using `-1` instead of `0`).

### CC-0097 — berendt
- **Feedback**: The PR description (Step 9 in Concrete steps) specifies updating architecture/docs/09-implementation/10-chaos-e2e-testing.md ... However `git diff main...HEAD -- architecture` is empty — the architecture submodule pointer is unchanged in this PR.
- **What was missed**: If the PR plan references doc/architecture changes outside this repo (e.g., in a git submodule), confirm either (a) the submodule pointer is bumped in this branch, or (b) the PR description / commit log links to the external commit/PR that captures the update. Empty `git diff` against the submodule path with no link is a red flag.
- **Fix**: Author added a commit linking to the external PR (commit bfc04619 'chore(CC-0097): link PR ...') in the commit log to record where the architecture doc change is captured.
