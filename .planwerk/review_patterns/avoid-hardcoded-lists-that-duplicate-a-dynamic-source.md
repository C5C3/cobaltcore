# Review Pattern: Avoid hardcoded lists that duplicate a dynamic source

**Review-Area**: architecture
**Detection-Hint**: When a workflow or config file contains a hardcoded list of items (releases, environments, services), check whether another file already derives the same list dynamically. Search for matrix generation steps or glob patterns that produce the same set.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Is the same set of values (e.g., release names) maintained in more than one place? Is there an existing dynamic mechanism (directory scan, generate-matrix job) that could be reused instead of a second hardcoded list?

## Why it matters

Hardcoded duplicates drift silently. Adding or removing a release requires touching multiple files, and forgetting one causes CI to test a stale set without any error signal.

## Examples from external reviews

### CC-0051 — sourcery-ai[bot]
- **Feedback**: The Tempest job's release matrix in `.github/workflows/ci.yaml` is now a hardcoded list of releases; consider deriving this from the same `generate-matrix` output used in `build-images.yaml` to avoid having to touch multiple places when adding or removing releases.
- **What was missed**: Is the same set of values (e.g., release names) maintained in more than one place? Is there an existing dynamic mechanism (directory scan, generate-matrix job) that could be reused instead of a second hardcoded list?
- **Fix**: Replaced the hardcoded matrix with a dynamic 'Generate Tempest release matrix' step that scans releases/*/ directories, deriving values via slug convention and outputting a tempest-releases JSON consumed via fromJson.
