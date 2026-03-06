# Review Pattern: Require shallow clone for external repository checkouts in CI

**Review-Area**: performance
**Detection-Hint**: When reviewing GitHub Actions workflows, check every `actions/checkout` step that fetches an external repository (i.e., has a `repository:` field pointing outside the project). If `fetch-depth: 1` is absent, the full history will be cloned on every run.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any `actions/checkout` usage with a `repository` parameter pointing to a third-party repo must include `fetch-depth: 1` unless the workflow explicitly needs git history (e.g., changelog generation, bisect). Especially flag large, long-lived upstream projects.

## Why it matters

External repositories like OpenStack Keystone carry over a decade of git history (hundreds of MB). Fetching this on every CI run wastes runner time, bandwidth, and disk space for data that is never used — `git apply` and Docker build contexts only need the working tree at the target ref.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The upstream service checkout (e.g. `openstack/keystone`) is performed without `fetch-depth: 1`, so the full git history is fetched. OpenStack Keystone has been active since 2012 — its full history is hundreds of MB of objects. Fetching it on every CI run wastes time and runner disk space, and the extra history is never used.
- **What was missed**: Any `actions/checkout` usage with a `repository` parameter pointing to a third-party repo must include `fetch-depth: 1` unless the workflow explicitly needs git history (e.g., changelog generation, bisect). Especially flag large, long-lived upstream projects.
- **Fix**: Added `fetch-depth: 1` to the `actions/checkout` step for `openstack/${{ matrix.service }}` to limit the clone to the tip commit at the resolved ref.
