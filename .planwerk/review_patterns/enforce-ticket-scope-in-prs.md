# Review Pattern: Enforce ticket scope in PRs

**Review-Area**: architecture
**Detection-Hint**: Scan the diff for references to ticket/issue IDs different from the PR's own ticket. Any added lines mentioning a different ticket number indicate scope creep.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Every changed line should relate to the ticket the PR is filed under. Look for references to other ticket IDs (e.g., CC-XXXX) in added documentation rows, comments, or code that do not match the current PR's scope.

## Why it matters

Scope creep makes PRs harder to review, harder to revert, and risks shipping unreviewed changes for other features. It also creates confusing git history where changes land under the wrong ticket.

## Examples from external reviews

### CC-0042 — berendt
- **Feedback**: The diff adds a networkPolicy row to the KeystoneSpec table referencing CC-0039, not CC-0042. This row does not exist on main and was introduced in this PR, constituting scope creep.
- **What was missed**: Every changed line should relate to the ticket the PR is filed under. Look for references to other ticket IDs (e.g., CC-XXXX) in added documentation rows, comments, or code that do not match the current PR's scope.
- **Fix**: Removed the networkPolicy documentation row from this PR so it can be added in the correct CC-0039 branch or a dedicated doc-only commit.

### CC-0050 — berendt
- **Feedback**: The useless-use-of-cat fix (replacing `cat | bao_exec_stdin` with input redirection) is unrelated to CC-0050. The file is not under `hack/`, not part of the CI workflow refactoring, and not covered by the shellcheck CI job.
- **What was missed**: Compare the list of changed files against the PR title/description scope. Flag any file that is not in the subsystem being refactored and whose change is not a necessary dependency of the main work.
- **Fix**: Reverted the unrelated changes in setup-policies.sh and verify_spdx_headers.sh from the PR to be submitted separately.

### CC-0086 — berendt
- **Feedback**: Commit bb51416 'fix(CC-0086): deflake operator-pod-crash with UID-based chaos check' rewrites the operator-pod-crash chaos test's Phase-1 polling logic (88 lines added, 26 removed). This has nothing to do with the Flux Web UI feature — the commit body explicitly calls it a port of the CC-0076 fix on operator-pod-kill.
- **What was missed**: Every commit's ticket prefix and touched file paths should align with the PR's declared feature/ticket. Flag commits that touch unrelated subsystems or reference different tickets in their body even if the prefix matches.
- **Fix**: Split the commit into a standalone fix(CC-0076): ... PR so the deflake can be reviewed and bisected independently of the feature work.
