# Review Pattern: Require immutable tags for published container images

**Review-Area**: security
**Detection-Hint**: Look for container image push steps that only use mutable tags like `:latest` without an accompanying immutable tag (git SHA, semver, digest). Check if overwritten tags leave no audit trail.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every `docker/build-push-action` (or equivalent push step) should include at least one immutable tag (e.g., `:${{ github.sha }}`) alongside any mutable convenience tags like `:latest`. Verify that PR builds don't silently overwrite production tags.

## Why it matters

Mutable-only tags destroy provenance: you cannot determine which commit produced a given image, cannot roll back to a prior version without hunting through workflow artifacts for digests, and cannot audit the supply chain.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: Both `python-base` and `venv-builder` are pushed with a single `:latest` tag (lines 53 and 66). Each workflow run overwrites the previous tag, including on PR builds. [...] there is no way to identify which commit produced the currently-published `:latest`, or to retrieve an earlier base image version without a digest from a workflow run artifact.
- **What was missed**: Every `docker/build-push-action` (or equivalent push step) should include at least one immutable tag (e.g., `:${{ github.sha }}`) alongside any mutable convenience tags like `:latest`. Verify that PR builds don't silently overwrite production tags.
- **Fix**: Added `ghcr.io/${{ steps.meta.outputs.owner }}/python-base:${{ github.sha }}` and `ghcr.io/${{ steps.meta.outputs.owner }}/venv-builder:${{ github.sha }}` tags alongside the existing `:latest` tags.
