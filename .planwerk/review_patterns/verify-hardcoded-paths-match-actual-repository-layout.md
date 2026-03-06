# Review Pattern: Verify hardcoded paths match actual repository layout

**Review-Area**: validation
**Detection-Hint**: When a script assigns a file path to a variable, trace that path from the expected working directory and confirm the file actually exists at that location in the repo tree.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Compare every hardcoded or constructed file path in scripts against the real directory structure. Pay special attention when one path variable (e.g., OVERRIDES) uses a subdirectory prefix like 'releases/${RELEASE}/' but a sibling variable (e.g., CONSTRAINTS) uses a bare filename — the inconsistency signals a bug.

## Why it matters

A wrong path causes the script to fail immediately on every invocation with a misleading error message, making the entire script non-functional. This is a ship-blocking defect that any manual test run would have caught.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `CONSTRAINTS` is set to the bare filename `upper-constraints.txt`, but the actual file lives at `releases/${RELEASE}/upper-constraints.txt` in the repository. When the script is invoked from the repo root as documented, the pre-flight check will immediately fail.
- **What was missed**: Compare every hardcoded or constructed file path in scripts against the real directory structure. Pay special attention when one path variable (e.g., OVERRIDES) uses a subdirectory prefix like 'releases/${RELEASE}/' but a sibling variable (e.g., CONSTRAINTS) uses a bare filename — the inconsistency signals a bug.
- **Fix**: Changed CONSTRAINTS from bare filename to 'releases/${RELEASE}/upper-constraints.txt' to match the OVERRIDES pattern and the actual repo layout.
