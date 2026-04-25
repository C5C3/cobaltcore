# Review Pattern: Verify CI path filters cover all referenced file dependencies

**Review-Area**: testing
**Detection-Hint**: For each CI job, trace all file references (e.g., `uses: ./.github/actions/...`, script paths) and confirm every referenced directory appears in the job's path filter list.
**Severity**: WARNING
**Occurrences**: 2

## What to check

When a workflow job uses composite actions or scripts from specific directories, check that those directories are included in the path-filter trigger so that changes to dependencies actually trigger the job.

## Why it matters

A missing path filter means changes to a shared composite action or script will not trigger the tests that depend on it, allowing broken changes to pass CI silently.

## Examples from external reviews

### CC-0054 — berendt
- **Feedback**: The `e2e_chaos` path filter does not include `.github/actions/**`. The `e2e-chaos` job references `./.github/actions/setup-e2e-infra` (ci.yaml:493). A change to the composite action will not trigger chaos E2E tests.
- **What was missed**: When a workflow job uses composite actions or scripts from specific directories, check that those directories are included in the path-filter trigger so that changes to dependencies actually trigger the job.
- **Fix**: Add `- '.github/actions/**'` to the `e2e_chaos` path filter list.

### CC-0088 — berendt
- **Feedback**: REQ-013 requires `curl -k https://keystone.127-0-0-1.nip.io/v3` to return HTTP 200... The suite contains a Keystone-CRD presence guard that exits 0 (SKIP) when the CRD is absent. The smoke suite only runs in the `e2e-infra` job... `deploy/kind/base/kustomization.yaml:106` patches the keystone-operator with `suspend: true`, so the Keystone CRD is never present in `e2e-infra`. As a result, REQ-013 is documented but unverified by any CI run.
- **What was missed**: For any test suite with conditional skip logic (e.g., 'skip if CRD missing'), follow the CI workflow path: which job runs this suite, and does that job's deploy step install the prerequisites the guard checks for? If the guard always trips in the only job that runs it, the test is dead code.
- **Fix**: Moved the smoke suite from `tests/e2e/infrastructure/gateway-quick-start-smoke/` to `tests/e2e/keystone/gateway-quick-start-smoke/` so the operator-installed `e2e-operator` matrix job picks it up, the CRD-presence guard passes, and the curl step actually runs.
