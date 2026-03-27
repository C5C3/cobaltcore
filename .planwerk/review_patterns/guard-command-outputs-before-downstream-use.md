# Review Pattern: Guard command outputs before downstream use

**Review-Area**: error-handling
**Detection-Hint**: Look for shell variable assignments from commands (yq, jq, grep, etc.) that are used in subsequent commands without null/empty checks. Compare with sibling extractions in the same file — if some have fallback guards but others don't, that's an inconsistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every shell variable populated by a parsing tool (yq, jq, awk) must be validated before use. Check that missing/null outputs produce a clear error message rather than a cryptic downstream failure.

## Why it matters

An unguarded null value propagates silently into downstream commands (e.g. git clone --branch null), producing confusing errors that waste debugging time and hide the actual root cause.

## Examples from external reviews

### CC-0018 — berendt
- **Feedback**: If the key for the matrix operator is absent from source-refs.yaml, yq outputs the literal string 'null'. The downstream git clone --branch null fails with a confusing remote ref error rather than pointing at the missing YAML key.
- **What was missed**: Every shell variable populated by a parsing tool (yq, jq, awk) must be validated before use. Check that missing/null outputs produce a clear error message rather than a cryptic downstream failure.
- **Fix**: Add an explicit null/empty check after the yq extraction and exit with a descriptive ::error:: message if SERVICE_REF is null or empty.
