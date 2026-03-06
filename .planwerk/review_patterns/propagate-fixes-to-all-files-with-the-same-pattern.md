# Review Pattern: Propagate fixes to all files with the same pattern

**Review-Area**: validation
**Detection-Hint**: When a PR fixes a bug in one file, search the codebase for all other files containing the same pattern (e.g., grep for 'apt-get install' across all Dockerfiles) and verify the fix was applied everywhere.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a fix addresses a specific code pattern (like adding DEBIAN_FRONTEND=noninteractive before apt-get), check whether the same vulnerable pattern exists in other files that were not modified in the PR.

## Why it matters

Partial fixes create a false sense of security. The same root cause (interactive prompts stalling CI) will resurface in the unfixed files, and the inconsistency makes it harder to spot in future reviews since the pattern was 'already fixed'.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `DEBIAN_FRONTEND=noninteractive` missing — same gap as the `python-base` fix. The same root cause was identified and fixed in `images/python-base/Dockerfile` (line 11), but the fix was not propagated here. Additionally, `images/venv-builder/Dockerfile` (line 11) has the same omission.
- **What was missed**: When a fix addresses a specific code pattern (like adding DEBIAN_FRONTEND=noninteractive before apt-get), check whether the same vulnerable pattern exists in other files that were not modified in the PR.
- **Fix**: DEBIAN_FRONTEND=noninteractive was added to both images/keystone/Dockerfile and images/venv-builder/Dockerfile to match the existing pattern in images/python-base/Dockerfile.
