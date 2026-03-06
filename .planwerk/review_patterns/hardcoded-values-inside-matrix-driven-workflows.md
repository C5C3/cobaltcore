# Review Pattern: Hardcoded values inside matrix-driven workflows

**Review-Area**: architecture
**Detection-Hint**: When reviewing a workflow or loop that uses a matrix/iteration variable, search for any literals that duplicate or assume a specific value of that variable. If the matrix is `[keystone]` today, check whether every step would still work if a second entry were added.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Every step inside a matrix job (or any parameterized loop) must derive service-specific values from the matrix variable, not hardcode them. Search for string literals that match any current matrix value — if found, they should likely reference the matrix variable instead.

## Why it matters

Hardcoded values silently break when the matrix is expanded. The workflow appears to work with a single-element matrix but fails or tests the wrong thing as soon as a second service is added, producing false confidence.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: Both this inline PR smoke-test step and the separate `smoke-test` job (line 193) hardcode `keystone-manage --version`. When the `service` matrix is extended beyond `[keystone]`, every run will attempt `keystone-manage --version` regardless of which service is actually being tested — causing failures for any non-keystone service.
- **What was missed**: Every step inside a matrix job (or any parameterized loop) must derive service-specific values from the matrix variable, not hardcode them. Search for string literals that match any current matrix value — if found, they should likely reference the matrix variable instead.
- **Fix**: Replaced `keystone-manage --version` with `${MATRIX_SERVICE}-manage --version` where `MATRIX_SERVICE` is set from `${{ matrix.service }}`, making the smoke test command dynamically derived from the matrix.
