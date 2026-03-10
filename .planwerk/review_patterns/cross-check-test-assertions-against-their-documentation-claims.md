# Review Pattern: Cross-check test assertions against their documentation claims

**Review-Area**: testing
**Detection-Hint**: When a PR includes both tests and documentation describing those tests, read the doc description of each test and verify the actual assertions match what the docs claim is validated.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For each test function, compare its actual assertions against any documentation table or description that enumerates what the test validates. Flag any claim in the docs that has no corresponding assertion in the test code.

## Why it matters

Documentation that overstates test coverage creates a false sense of safety. Teams will trust the docs and skip manual verification of properties they believe are already tested, allowing regressions to slip through.

## Examples from external reviews

### CC-0032 — greptile-apps[bot]
- **Feedback**: Both `test_grype_scan_action_sha_pinned` and `test_sarif_upload_action_sha_pinned` validate only the 40-character hex SHA pattern, but do not check for the inline version comments (`# v7` and `# v3`). The documentation table explicitly states that these tests validate the version comments.
- **What was missed**: For each test function, compare its actual assertions against any documentation table or description that enumerates what the test validates. Flag any claim in the docs that has no corresponding assertion in the test code.
- **Fix**: Added `assert_file_contains` calls to validate the `# v7` and `# v3` version comments in the respective test functions, aligning actual test behavior with the documentation.
