# Review Pattern: Enforce consistent error-handling pattern across all test cases

**Review-Area**: testing
**Detection-Hint**: When a file already uses a defensive pattern (e.g., capturing exit codes with `|| exit_code=$?`) in some test cases, scan ALL other test cases in the same file for the same invocation pattern without the guard.
**Severity**: BLOCKING
**Occurrences**: 7

## What to check

Under `set -euo pipefail`, any command expected to succeed but invoked without exit-code capture will abort the entire test script on unexpected failure — skipping all assertions and the results summary. Check that every invocation of the script-under-test uses the same idiom consistently.

## Why it matters

Silent test abortion means CI reports a failure with zero diagnostic output. Developers waste time debugging the test harness instead of the actual failure. Worse, partial runs can mask which tests actually passed.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Tests 2, 3, 4, 5, and 7 invoke the script-under-test without `|| exit_code=$?` ... Under `set -euo pipefail`, if the script exits non-zero ... the test script aborts immediately — before any `assert_file_contains` / `assert_file_not_contains` calls run, and before the `=== Results ===` summary is ever printed.
- **What was missed**: Under `set -euo pipefail`, any command expected to succeed but invoked without exit-code capture will abort the entire test script on unexpected failure — skipping all assertions and the results summary. Check that every invocation of the script-under-test uses the same idiom consistently.
- **Fix**: All test cases were updated to use `local exit_code=0; (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?; assert_eq "script exits 0" "0" "$exit_code"`, matching the pattern already present in Tests 1 and 6.

### CC-0027 — greptile-apps[bot]
- **Feedback**: The new test confirms that the Dockerfile declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES`, and that the CI workflow references `extra-packages.yaml`. However, it has no check for `ARG EXTRA_APT_PACKAGES` in Stage 2 of the Dockerfile. Had this check existed, it would have caught that the apt-packages half of the wiring is missing.
- **What was missed**: List all build args or config keys the PR introduces. For each one, verify the test suite includes an assertion that the arg is declared and used. If the test checks 2 of 3 args, the unchecked arg is the one most likely to be broken.
- **Fix**: Added an `ARG EXTRA_APT_PACKAGES` grep check to `test_extra_packages_build_wiring` and a new `test_no_hardcoded_apt_packages` test to verify the runtime stage uses the build arg instead of hardcoded values.

### CC-0029 — greptile-apps[bot]
- **Feedback**: `test_sbom_format_cyclonedx_json` has asymmetric handling of empty results: the base-images branch uses a silent non-count pattern, while the service branch correctly uses `assert_eq`.
- **What was missed**: Parallel code paths within the same test function should use the same assertion strategy. If one branch uses assert_eq (which handles empty/mismatch as FAIL), all equivalent branches must do the same or provide equivalent explicit failure handling.
- **Fix**: Added an elif branch to the base-images block to explicitly fail when base_formats is empty, making it consistent with the service branch that already used assert_eq.

### CC-0056 — berendt
- **Feedback**: The project has 18 Chainsaw E2E test directories under tests/e2e/keystone/ covering every existing feature. This PR adds a significant new feature (upgrade flow) but includes no Chainsaw E2E test.
- **What was missed**: List the existing test directories (e.g. tests/e2e/keystone/*). If the PR introduces a new feature or flow, verify a corresponding test directory and chainsaw-test.yaml (or equivalent) is included.
- **Fix**: Created tests/e2e/keystone/upgrade-flow/ with chainsaw-test.yaml covering fresh deployment, sequential upgrade, and skip-level rejection scenarios.

### CC-0063 — berendt
- **Feedback**: The project has 19 Chainsaw E2E test directories under tests/e2e/keystone/ covering every existing feature [...]. This PR adds a new operational feature but includes no corresponding E2E test that asserts the Deployment spec fields (terminationGracePeriodSeconds, startupProbe, lifecycle.preStop) on a real cluster.
- **What was missed**: Check whether the project has a consistent 1-feature-to-1-E2E-test-directory convention. If it does, verify the PR includes a new test directory that asserts the feature's spec fields on a real cluster, not just unit/integration coverage.
- **Fix**: Added a tests/e2e/keystone/graceful-shutdown/ directory with chainsaw-test.yaml that applies a Keystone CR and asserts the Deployment spec contains the expected terminationGracePeriodSeconds, startupProbe, and lifecycle.preStop fields.

### CC-0069 — berendt
- **Feedback**: The test suite has 26 negative test cases verifying that invalid values are rejected, but zero positive test cases verifying that valid non-default values are accepted through schema validation. If a future schema edit accidentally over-restricts a field (e.g., adding a maxLength that's too short, or a pattern that rejects a valid quantity), no test in this file would catch the regression.
- **What was missed**: Schema validation test suites must include positive test cases that verify valid non-default values pass through the schema. Check that for each constrained field (patterns, ranges, enums), at least one test asserts a valid non-default value renders successfully.
- **Fix**: Added 5 positive schema test cases covering custom replicas, custom metrics port, string resource quantities, numeric resource quantities, and the namespaceScoped+webhook combination, each asserting successful template rendering with isKind.

### CC-0077 — berendt
- **Feedback**: The project has 25 Chainsaw E2E test directories under tests/e2e/keystone/ covering every existing feature. This PR adds a new operational behavior (pruning) but includes no tests/e2e/keystone/config-pruning/ directory.
- **What was missed**: Count existing E2E test directories and compare against the feature set. If a new operational behavior is introduced without a matching E2E test directory, flag it. Look at the test directory structure (e.g., tests/e2e/) for the established convention.
- **Fix**: Created tests/e2e/keystone/config-pruning/chainsaw-test.yaml with a 00-keystone-cr.yaml fixture, covering CR deploy, 4 sequential config patches, DeploymentReady waits, and a final assertion that ConfigMap count is at most retain+1=4.
