# Review Pattern: Verify documentation headings match actual dependency graph

**Review-Area**: documentation
**Detection-Hint**: When CI workflow documentation groups jobs under a heading that implies a specific trigger mechanism (e.g., 'path-filtered'), cross-reference each listed job against the actual workflow YAML to confirm it truly uses that mechanism.
**Severity**: WARNING
**Occurrences**: 9

## What to check

For each job listed under a conditional/filtered section in CI docs, verify the job actually has the stated dependency (needs, if-condition, path filter). Any job that doesn't match the heading's description must be moved to its own section.

## Why it matters

Misleading DAG documentation causes engineers to misunderstand which jobs are gated and which run unconditionally, leading to incorrect assumptions about CI behavior and wasted debugging time.

## Examples from external reviews

### CC-0041 — berendt
- **Feedback**: The docs job is placed under the heading 'Conditional Jobs (path-filtered via changes job)' but it does NOT depend on the changes job and is NOT path-filtered. This is inaccurate and misleading.
- **What was missed**: For each job listed under a conditional/filtered section in CI docs, verify the job actually has the stated dependency (needs, if-condition, path filter). Any job that doesn't match the heading's description must be moved to its own section.
- **Fix**: Split the section so path-filtered jobs stay under 'Conditional Jobs' and the docs job gets its own 'Independent Jobs' heading.

### CC-0060 — berendt
- **Feedback**: The PR updates the count from 9 to 10, but there are actually 20 test suite directories under tests/e2e/keystone/. The same stale count appears on line 59. Avoid hardcoding a count that will go stale with every new test PR.
- **What was missed**: Search the diff for changes to numeric literals in prose text. For each changed number, verify it matches the actual count of the referenced items (directories, files, endpoints, etc.). Also search for other occurrences of the old number in the same file.
- **Fix**: Removed the hardcoded '10' from both locations, replacing with generic language: 'The test suites...' and 'All test suites...'.

### CC-0072 — berendt
- **Feedback**: The PR description states that individual sub-reconcilers are inconsistent — some set ObservedGeneration, others omit it. However, git diff main...HEAD -- '*.go' ':!*_test.go' produces no output — the production code on main already sets ObservedGeneration on all 49 SetCondition calls across all 13 reconciler files.
- **What was missed**: Run the diff filtered to production files (exclude tests and docs). If the diff is empty but the description claims a production fix, the description is misleading about the current state of the codebase.
- **Fix**: Updated the PR description to clarify that production code was already standardized in prior feature PRs and this ticket adds the missing test coverage and documents the convention.

### CC-0074 — sourcery-ai[bot]
- **Feedback**: The doc comment for `TestBuildKeystoneDeployment_StablePodTemplate` says it verifies stability across invocations with different Secret contents, but the test calls `buildKeystoneDeployment` twice with identical inputs.
- **What was missed**: Verify that test names and doc comments accurately describe what the test does. Specifically check: do the test inputs actually differ in the way the comment claims? Does the test exercise the scenario it says it exercises?
- **Fix**: Reworded the doc comment to clarify the test only verifies deterministic output for identical inputs, and noted that differing Secret contents must be covered by higher-level reconciliation tests.

### CC-0079 — berendt
- **Feedback**: The documentation describes a three-value return shape for finalizeOpenBaoSecrets that does not exist in the code. The actual signature is (done bool, err error) only; the stuck object's name is recorded as a side effect on keystone.Status.Conditions via setOpenBaoFinalizerBlockedCondition, not returned as a third value.
- **What was missed**: Every function signature, return tuple, and parameter list mentioned in prose or code blocks in docs must match the current implementation. Watch for stale extra return values and side effects described as return values.
- **Fix**: Rewrote the docs to describe the real (done bool, err error) signature and documented the stuck-name recording as a side effect on keystone.Status.Conditions via setOpenBaoFinalizerBlockedCondition.

### CC-0089 — berendt
- **Feedback**: The doc comment at line 70-72 claims the drift-guard prevents this fallback from firing in practice, but the drift guard does not actually catch this direction.
- **What was missed**: When code comments reference safety guarantees provided by tests or guards (e.g., 'this fallback never fires because of test X'), verify the cited test actually enforces the claimed invariant in the relevant direction.
- **Fix**: Doc comments on the constant, the map, and the helper were updated to describe the dual static-guard + runtime-sentinel strategy.

### CC-0094 — berendt
- **Feedback**: The chainsaw-test.yaml comment, CI workflow comment, and docs all claim the job 'fails the build before the cluster-bound e2e-operator job runs'. However, the verify-invalid-cr-fixtures job has no needs: block and is not listed in needs: of build-e2e-images or e2e-operator. They run in parallel; the only 'gating' is the overall PR fail status.
- **What was missed**: For every claim in code comments, workflow comments, or docs that job A 'fails the build before job B runs' or 'gates' job B, verify job B (and any transitive dependents) actually has job A in its `needs:` list.
- **Fix**: Added `verify-invalid-cr-fixtures` to the `needs:` of `build-e2e-images` so the documented gating of `e2e-operator` actually holds.

### CC-0098 — berendt
- **Feedback**: PR description Step 8 and the Acceptance Criteria explicitly list a cross-link in architecture/docs/.../02-logging.md as a deliverable, but `git diff origin/main...HEAD -- architecture/` is empty. The submodule pointer is unchanged in this branch and the PR description / commit log contains no link to an external commit/PR capturing the update.
- **What was missed**: For each file or path the PR description promises to touch, confirm it appears in the diff. For submodule directories with empty diffs, require a commit-message link to the upstream PR/commit that captures the change.
- **Fix**: Added commit `chore(CC-0098): link PR https://github.com/C5C3/forge/pull/298` (6618a8ca) pointing at the upstream architecture commit that captures the cross-link update.

### CC-0104 — berendt
- **Feedback**: REQ-003 (SHALL) requires plain Kubernetes Secrets in the test namespace, but the implementation uses ExternalSecrets. The spec's literal requirement is unimplementable: reconcileSecrets calls WaitForExternalSecret... The deviation is documented in the file header (lines 14-25) but the spec was not updated to match.
- **What was missed**: When a PR implements a planwerk spec, verify each SHALL/MUST requirement in the spec literally matches what the code does. If the implementation diverges (e.g., uses ExternalSecrets where spec says plain Secrets), the spec must be amended or the deviation formally recorded — don't accept silent drift documented only in file headers.
- **Fix**: Documented the deviation as a resolved planwerk-record (.planwerk/reviews/CC-0104-github-review-by-berendt-external-1.json) and corrected inline doc comments in 01-keystone-cr.yaml from 'plain Secrets' to ExternalSecrets to match reality.
