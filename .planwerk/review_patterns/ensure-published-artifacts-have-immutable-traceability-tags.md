# Review Pattern: Ensure published artifacts have immutable traceability tags

**Review-Area**: architecture
**Detection-Hint**: When a CI workflow pushes container images or artifacts with only a mutable tag like `:latest`, check whether there is also an immutable tag (commit SHA, digest) that allows correlating the published artifact back to its source commit.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Intermediate/base images pushed with only `:latest` or other mutable tags. Verify there is at least one immutable tag (e.g., `:<sha>`) or that the digest is persisted somewhere retrievable, so you can audit which commit produced a given artifact and roll back if needed.

## Why it matters

Without an immutable tag, there is no way to determine which commit produced the currently-published image or retrieve a previous version without digging through workflow run logs for digests. This hampers incident response, auditing, and rollback.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: Both `python-base` and `venv-builder` are pushed with a single `:latest` tag. Each workflow run overwrites the previous tag... there is no way to identify which commit produced the currently-published `:latest`, or to retrieve an earlier base image version without the digest from a workflow run artifact.
- **What was missed**: Intermediate/base images pushed with only `:latest` or other mutable tags. Verify there is at least one immutable tag (e.g., `:<sha>`) or that the digest is persisted somewhere retrievable, so you can audit which commit produced a given artifact and roll back if needed.
- **Fix**: Added an immutable `:${{ github.sha }}` tag alongside `:latest` for base images, enabling commit-to-image traceability.
