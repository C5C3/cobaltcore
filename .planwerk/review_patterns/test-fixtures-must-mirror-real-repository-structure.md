# Review Pattern: Test fixtures must mirror real repository structure

**Review-Area**: testing
**Detection-Hint**: When reviewing test setup code that creates temporary directories and files, compare the directory structure created in the test workdir against the actual repository layout. If the test places files at different relative paths than production, it will mask path-related bugs.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Check that test workdir file placement (e.g., '$workdir/upper-constraints.txt') matches where the file lives in the real repo (e.g., '$workdir/releases/2025.2/upper-constraints.txt'). A test that passes with a flat structure but would fail against the real repo tree is a false-positive test.

## Why it matters

Tests that use a simplified directory structure give false confidence — they pass even though the code under test would fail in every real invocation. The bug in the script went undetected precisely because the tests accommodated the wrong path.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Each test workdir places `upper-constraints.txt` directly in the temp dir root rather than under `releases/2025.2/upper-constraints.txt`. Once `CONSTRAINTS` is corrected, the test setup should be updated to stay consistent with the real repository structure.
- **What was missed**: Check that test workdir file placement (e.g., '$workdir/upper-constraints.txt') matches where the file lives in the real repo (e.g., '$workdir/releases/2025.2/upper-constraints.txt'). A test that passes with a flat structure but would fail against the real repo tree is a false-positive test.
- **Fix**: Updated test setup to create files under 'releases/2025.2/' subdirectory in the test workdir, matching the real repository layout.
