# Review Pattern: Match string comparison semantics to domain rules

**Review-Area**: validation
**Detection-Hint**: When reviewing code that matches or filters identifiers from an external ecosystem (package names, email addresses, hostnames), verify whether that ecosystem treats identifiers as case-sensitive or case-insensitive, and confirm the matching logic (grep, sed, string comparison) matches that semantic.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Shell scripts using sed or grep to match package names against constraint files must account for case-insensitivity (PyPI package names are case-insensitive). A case-sensitive sed pattern like /^pymysql===/d will fail to match PyMySQL===, leaving duplicate conflicting constraints.

## Why it matters

Silent mismatches produce duplicate conflicting entries that cause downstream build failures in pip/uv resolution. The bug is hard to diagnose because the script exits successfully and the constraint file looks plausible on casual inspection.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Case-sensitive package matching can silently miss existing constraints. If an override file writes `pymysql===2.1.0` but the constraint file contains `PyMySQL===2.0.0`, the sed pattern will not match, creating a duplicate with conflicting versions.
- **What was missed**: Shell scripts using sed or grep to match package names against constraint files must account for case-insensitivity (PyPI package names are case-insensitive). A case-sensitive sed pattern like /^pymysql===/d will fail to match PyMySQL===, leaving duplicate conflicting constraints.
- **Fix**: Case-insensitive matching (/Id flag) was added to both sed calls in apply-constraint-overrides.sh, and a new test was added covering the case-mismatch scenario.
