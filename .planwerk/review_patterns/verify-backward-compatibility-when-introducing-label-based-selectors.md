# Review Pattern: Verify backward compatibility when introducing label-based selectors

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new label AND uses that label as a filter/selector in the same PR, ask: what happens to resources created before this label existed?
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check whether any List/Get/selector operation introduced in the PR relies on a label that is also first introduced in the same PR. If so, pre-existing resources will be invisible to that selector, creating a silent data gap.

## Why it matters

Pre-existing resources that lack the new label will never match the selector, causing silent accumulation or missed processing. The feature appears to work in tests (which only create new resources) but fails on upgrade in production.

## Examples from external reviews

### CC-0077 — berendt
- **Feedback**: ConfigMaps created before this PR is deployed will lack the forge.c5c3.io/config-base label, so the label-based List will never find them. These pre-existing unlabeled ConfigMaps will accumulate forever and never be pruned.
- **What was missed**: Check whether any List/Get/selector operation introduced in the PR relies on a label that is also first introduced in the same PR. If so, pre-existing resources will be invisible to that selector, creating a silent data gap.
- **Fix**: Documented as a known limitation in the PruneImmutableConfigMaps godoc (config.go:221-226), noting that pre-existing unlabeled ConfigMaps are bounded and will be garbage-collected when the owner CR is deleted.
