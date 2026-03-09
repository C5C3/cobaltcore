# Review Pattern: Check mutable tags for cross-branch clobbering

**Review-Area**: architecture
**Detection-Hint**: When a workflow pushes container images or artifacts with version-only tags (e.g., `keystone:28.0.0`) and triggers from multiple branches, check whether concurrent or sequential builds from different branches can silently overwrite the same tag.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any image tag or artifact identifier that does not include branch information but is pushed from multiple branches (e.g., both `main` and `stable/**`). The same upstream version built from different branches will produce different images under an identical tag — last writer wins silently.

## Why it matters

A deployment system referencing a version-only tag could silently receive an image built from an unexpected branch, leading to difficult-to-diagnose inconsistencies in production where the image content doesn't match expectations.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The standalone version tag (e.g., `keystone:28.0.0`) is pushed unconditionally on every push event, from any branch. Since both `main` and `stable/**` branches can build the same upstream version (`28.0.0`), concurrent or sequential builds from different branches will overwrite each other's `keystone:28.0.0` tag — the last writer wins silently.
- **What was missed**: Any image tag or artifact identifier that does not include branch information but is pushed from multiple branches (e.g., both `main` and `stable/**`). The same upstream version built from different branches will produce different images under an identical tag — last writer wins silently.
- **Fix**: Restricted the mutable version-only tag push to `main` branch only, preventing cross-branch clobbering.
