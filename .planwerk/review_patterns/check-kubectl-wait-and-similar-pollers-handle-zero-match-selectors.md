# Review Pattern: Check kubectl wait and similar pollers handle zero-match selectors

**Review-Area**: error-handling
**Detection-Hint**: Any `kubectl wait` call (or equivalent polling command) used after an async resource creation step. Check whether the wait is invoked in a window where matching resources may not yet exist.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a script waits for a condition on resources selected by label, verify that the wait handles the case where zero resources currently match the selector. `kubectl wait` fails immediately (not waits) when no pods match. With `set -e`, this aborts the script.

## Why it matters

Creates a race condition that causes intermittent CI failures. The script works most of the time (when pods appear quickly) but fails unpredictably on slow runners, producing no useful diagnostics.

## Examples from external reviews

### CC-0010 — greptile-apps[bot]
- **Feedback**: `kubectl wait` exits with a non-zero status code when no pods match the label selector — it does not wait for pods to appear; it fails immediately. With `set -e` active, this aborts the entire deployment.
- **What was missed**: When a script waits for a condition on resources selected by label, verify that the wait handles the case where zero resources currently match the selector. `kubectl wait` fails immediately (not waits) when no pods match. With `set -e`, this aborts the script.
- **Fix**: Rewrote `wait_for_pods` to poll for pod existence with `kubectl get pods` before handing off to `kubectl wait`.
