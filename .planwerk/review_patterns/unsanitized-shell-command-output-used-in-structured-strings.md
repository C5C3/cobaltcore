# Review Pattern: Unsanitized shell command output used in structured strings

**Review-Area**: validation
**Detection-Hint**: When shell command output (e.g., `wc`, `awk`, `cut`) is interpolated into a structured string like a Docker tag, URL, or filename, check whether the output could contain unexpected whitespace, newlines, or special characters. Pipe through `xargs`, `tr -d`, or `sed` to trim.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Identify shell pipelines whose output is assigned to variables later used in Docker tags, file paths, API parameters, or other structured contexts. Verify the output is trimmed/sanitized — especially `wc` (leading spaces on some platforms), `awk` (trailing newlines), and any command whose format varies across GNU vs BSD coreutils.

## Why it matters

Leading/trailing whitespace in a Docker tag or similar identifier produces silently invalid values. The issue may not manifest on the CI platform (Ubuntu) but breaks local testing on macOS or other environments, causing hard-to-debug failures.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: `wc -l` output format differs across platforms: on macOS/BSD it pads the count with leading spaces (e.g., `       3`), while GNU coreutils on Ubuntu omits the padding when reading from stdin... the resulting `PATCH_COUNT` would contain spaces, producing an invalid Docker tag like `keystone:28.0.0-p       3-main-abc1234`.
- **What was missed**: Identify shell pipelines whose output is assigned to variables later used in Docker tags, file paths, API parameters, or other structured contexts. Verify the output is trimmed/sanitized — especially `wc` (leading spaces on some platforms), `awk` (trailing newlines), and any command whose format varies across GNU vs BSD coreutils.
- **Fix**: Added `| xargs` after `wc -l` to trim whitespace: `PATCH_COUNT=$(find "$PATCH_DIR" -name '*.patch' -type f | wc -l | xargs)`.

### CC-0007 — greptile-apps[bot]
- **Feedback**: If the key for `${MATRIX_SERVICE}` is absent from `releases/${MATRIX_RELEASE}/source-refs.yaml` — which happens whenever a new service is added to the matrix before its entry is added to the YAML file — `yq` outputs the string `"null"`. That string is written verbatim to `GITHUB_OUTPUT` and then passed as `ref:` to `actions/checkout` on the next step, which fails with a cryptic `couldn't find remote ref null` error rather than a clear "key not found in source-refs.yaml" message.
- **What was missed**: When a CLI tool's output is stored in a variable and consumed downstream, verify there is an explicit guard for missing/invalid output (empty string, literal 'null', unexpected format) with a clear error message before the value propagates.
- **Fix**: Added a guard after the yq call: `if [ -z "$ref" ] || [ "$ref" = "null" ]; then echo "::error::No source-ref found for ..."; exit 1; fi`. Applied the same guard to a second unvalidated yq call in the smoke-test job.
