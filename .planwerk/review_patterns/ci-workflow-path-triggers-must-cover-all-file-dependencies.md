# Review Pattern: CI workflow path triggers must cover all file dependencies

**Review-Area**: architecture
**Detection-Hint**: When a workflow references composite actions (.github/actions/**), scripts (hack/), or other external files, check that every referenced path is included in the on.push.paths and on.pull_request.paths triggers.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For each uses: ./.github/actions/* or script invocation (hack/*, scripts/*) in the workflow, verify a matching glob exists in both path trigger blocks.

## Why it matters

A bug fix in a composite action or helper script will not trigger the workflow, so it ships untested and potentially broken to the default branch.

## Examples from external reviews

### CC-0055 — berendt
- **Feedback**: The workflow now depends on 8 composite actions under .github/actions/ and 5 scripts under hack/ci-*. A change to any of these files will not trigger the workflow.
- **What was missed**: For each uses: ./.github/actions/* or script invocation (hack/*, scripts/*) in the workflow, verify a matching glob exists in both path trigger blocks.
- **Fix**: Add `.github/actions/**` and `hack/ci-*` to both `paths:` blocks in the push and pull_request triggers.
