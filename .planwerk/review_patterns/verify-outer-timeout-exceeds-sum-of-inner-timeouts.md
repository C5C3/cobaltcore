# Review Pattern: Verify outer timeout exceeds sum of inner timeouts

**Review-Area**: testing
**Detection-Hint**: When a CI job or wrapper script sets a timeout, add up all the internal/script-level timeouts and compare. The outer timeout must be strictly greater than the sum of inner timeouts plus estimated setup overhead.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Sum all configured wait/timeout values inside the scripts the job runs. Add estimated overhead for setup steps (cluster creation, image pulls, installs). Confirm the job-level `timeout-minutes` exceeds this total by a reasonable margin.

## Why it matters

When the outer timeout fires first, the runner is killed immediately — internal timeout logic never executes its diagnostic output (e.g., `kubectl get helmreleases`), making failures much harder to debug.

## Examples from external reviews

### CC-0010 — greptile-apps[bot]
- **Feedback**: Script-level waits alone total 1200 seconds = 20 minutes. On top of that, cluster creation, binary downloads, flux install... add another 5–10 minutes. When the GitHub Actions timeout fires, the runner is killed immediately.
- **What was missed**: Sum all configured wait/timeout values inside the scripts the job runs. Add estimated overhead for setup steps (cluster creation, image pulls, installs). Confirm the job-level `timeout-minutes` exceeds this total by a reasonable margin.
- **Fix**: Raised CI job `timeout-minutes` from 20 to 40.
