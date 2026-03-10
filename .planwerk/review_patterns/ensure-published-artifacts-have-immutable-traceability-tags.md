# Review Pattern: Ensure published artifacts have immutable traceability tags

**Review-Area**: architecture
**Detection-Hint**: When a CI workflow pushes container images or artifacts with only a mutable tag like `:latest`, check whether there is also an immutable tag (commit SHA, digest) that allows correlating the published artifact back to its source commit.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Intermediate/base images pushed with only `:latest` or other mutable tags. Verify there is at least one immutable tag (e.g., `:<sha>`) or that the digest is persisted somewhere retrievable, so you can audit which commit produced a given artifact and roll back if needed.

## Why it matters

Without an immutable tag, there is no way to determine which commit produced the currently-published image or retrieve a previous version without digging through workflow run logs for digests. This hampers incident response, auditing, and rollback.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: Both `python-base` and `venv-builder` are pushed with a single `:latest` tag. Each workflow run overwrites the previous tag... there is no way to identify which commit produced the currently-published `:latest`, or to retrieve an earlier base image version without the digest from a workflow run artifact.
- **What was missed**: Intermediate/base images pushed with only `:latest` or other mutable tags. Verify there is at least one immutable tag (e.g., `:<sha>`) or that the digest is persisted somewhere retrievable, so you can audit which commit produced a given artifact and roll back if needed.
- **Fix**: Added an immutable `:${{ github.sha }}` tag alongside `:latest` for base images, enabling commit-to-image traceability.

### CC-0011 — greptile-apps[bot]
- **Feedback**: The `//go:generate` directive here regenerates the CRD YAML but does not generate the webhook configuration YAML. The `make manifests` target runs two controller-gen commands — one for CRD, one for webhook — but `go generate ./...` from within this package only runs the CRD command.
- **What was missed**: Compare the set of controller-gen commands in go:generate directives against the set in the Makefile's manifest target. Check for missing generation steps (e.g., webhook, RBAC) and flag inconsistent output path syntax (e.g., `output:crd:dir=` vs canonical `output:crd:artifacts:config=`).
- **Fix**: Added a `//go:generate controller-gen webhook paths=. output:webhook:artifacts:config=../../config/webhook` directive and updated the CRD directive to use the canonical `output:crd:artifacts:config=` form matching the Makefile.
