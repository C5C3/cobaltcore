# Review Pattern: Enforce consistent patterns across sibling functions

**Review-Area**: validation
**Detection-Hint**: When a new function follows the same structure as existing sibling functions (same loop+summary pattern, same counter logic), diff the control flow against the existing functions. Any deviation in branching (e.g., `else` vs `elif`) is a red flag.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When reviewing a function that mirrors the structure of nearby functions in the same file, compare the branching and counter logic line-by-line. If sibling functions use `elif [ -z "$var" ]` but the new function uses a bare `else`, flag the inconsistency and verify correctness.

## Why it matters

Inconsistency between sibling functions that follow the same pattern is a strong signal of a copy-paste bug. The reviewer already had correct reference implementations in the same file — the bug was catchable by simple comparison.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: Both the `test_sbom_format_cyclonedx_json` and `test_sbom_attestation_push_to_registry` functions correctly avoid this by using `elif [ -z "$base_formats" ]` / `elif [ -z "$base_push_values" ]` instead of a plain `else`, so only the truly unhandled "no results" case increments an extra FAIL. This function should follow the same pattern.
- **What was missed**: When reviewing a function that mirrors the structure of nearby functions in the same file, compare the branching and counter logic line-by-line. If sibling functions use `elif [ -z "$var" ]` but the new function uses a bare `else`, flag the inconsistency and verify correctness.
- **Fix**: Aligned both the `base_sbom_ifs` and `service_sbom_ifs` blocks to use the same `elif [ -z ... ]` pattern as the other test functions in the file.
