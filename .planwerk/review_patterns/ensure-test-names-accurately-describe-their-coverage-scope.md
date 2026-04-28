# Review Pattern: Ensure test names accurately describe their coverage scope

**Review-Area**: testing
**Detection-Hint**: When a test name contains universal quantifiers like 'All', 'Every', 'Runs all', or 'Full', verify that every branch or validation path in the function under test is actually exercised. Cross-reference the test setup with the production code's branches.
**Severity**: WARNING
**Occurrences**: 7

## What to check

For tests claiming comprehensive coverage (e.g., 'RunsAllValidations'), enumerate all validation branches in the function under test and confirm each one is triggered in the test input. Check that the assertion count or error substring checks match the number of distinct validation paths.

## Why it matters

Misleadingly named tests give false confidence that all paths are covered and that error accumulation (no short-circuiting) is proven. Future developers may skip writing tests for uncovered paths, assuming they are already tested.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The test is named to imply every validation rule is exercised simultaneously, but it only mutates Replicas, RotationSchedule, Plugins, and PolicyOverrides. The two other defense-in-depth checks — cache mutual-exclusivity (REQ-009) and database mutual-exclusivity (REQ-010) — are never put into a broken state here.
- **What was missed**: For tests claiming comprehensive coverage (e.g., 'RunsAllValidations'), enumerate all validation branches in the function under test and confirm each one is triggered in the test input. Check that the assertion count or error substring checks match the number of distinct validation paths.
- **Fix**: Expanded TestValidateCreate_RunsAllValidations to include cache and database mutual-exclusivity violations with assertions for both 'cache' and 'database' error substrings, so it now exercises all validation paths.

### CC-0014 — greptile-apps[bot]
- **Feedback**: `TestSimulateMariaDBReady_zeroReplicas` exists for `SimulateMariaDBReady`, which ensures the zero-replica edge case is handled. `SimulateDeploymentReady` doesn't have an equivalent.
- **What was missed**: Does a newly added function have the same edge-case test coverage as its analogous siblings in the same package? Specifically check for zero-value and boundary-condition tests.
- **Fix**: Added `TestSimulateDeploymentReady_zeroReplicas` that creates a deployment, calls `SimulateDeploymentReady` with 0 replicas, and asserts `ReadyReplicas` is 0.

### CC-0016 — berendt
- **Feedback**: Every test CR specifies database: keystone in its spec. With parallel: 4, up to 4 concurrent keystone-manage db_sync Jobs will execute Alembic migrations against the same database simultaneously.
- **What was missed**: For each resource name (database schema, queue, bucket, etc.) referenced in test fixtures, verify it is unique per test suite when tests run in parallel. Search for hardcoded shared names like `database: keystone` appearing across multiple test directories.
- **Fix**: Assigned unique database names per test (e.g., `keystone_basic`, `keystone_scale`, `keystone_cleanup`) across all 9 CR fixture files.

### CC-0042 — berendt
- **Feedback**: The project has Chainsaw E2E tests for every existing feature (13 directories under tests/e2e/keystone/), but this PR adds no E2E test for the resources feature.
- **What was missed**: When every existing feature has a corresponding test directory or test suite (e.g., 13 out of 13 features have tests/e2e/ directories), a PR adding a new feature must include the same kind of test. Absence is a gap, not an oversight to defer.
- **Fix**: Created tests/e2e/keystone/resources/ with a chainsaw-test.yaml covering default resource propagation and custom resource patching.

### CC-0072 — berendt
- **Feedback**: reconcile_database.go contains approximately 15 distinct SetCondition calls across paths like ClusterNotReady, WaitingForDatabase, WaitingForUser, etc. The test covers only 2 paths (one True, one False). If someone removes ObservedGeneration from an untested path, no test catches it.
- **What was missed**: Count the distinct code paths in production that set the property under test. Compare to the number of paths exercised in the new tests. If coverage is below ~30% of paths, flag that regressions in untested paths will go undetected.
- **Fix**: Expanded reconcile_database_test.go from 2 to 5 tested condition paths (added WaitingForDatabase, DBSyncFailed, SchemaDriftDetected) and reconcile_secrets_test.go from 2 to 4 paths (added WaitingForDBCredentials, WaitingForAdminCredentials).

### CC-0048 — berendt
- **Feedback**: The Job `chaos-cron-test` is created via a `script` step, which means Chainsaw does not track it for automatic cleanup. After the test completes, this Job remains in the `openstack` namespace. On a subsequent test run, Step 4 will fail with `AlreadyExists`.
- **What was missed**: When a test step creates a named resource via a shell script rather than through the test framework's declarative resource management, verify that (1) a pre-creation cleanup with --ignore-not-found or equivalent guards against stale leftovers, and (2) a post-test cleanup step exists. Without both, the test breaks on re-run.
- **Fix**: Add `kubectl delete job chaos-cron-test -n $NAMESPACE --ignore-not-found` before `kubectl create job` to make the step idempotent.

### CC-0092 — berendt
- **Feedback**: The unit test TestPushSecretToKeystoneMapper_EmptyNamespaceReturnsNil was renamed to TestPushSecretToKeystoneMapper_NoKeystonesInNamespaceReturnsNil — the prior name asserted a behavior the code does not enforce.
- **What was missed**: Test function names like TestX_EmptyNamespaceReturnsNil should actually exercise the empty-namespace path. If the input is precluded by an upstream invariant (apiserver, validation), the test name should describe what is really being asserted.
- **Fix**: Renamed the test and updated the comment to clarify the empty-namespace case is precluded by the apiserver, so the test actually covers 'no Keystones in namespace returns nil'.
