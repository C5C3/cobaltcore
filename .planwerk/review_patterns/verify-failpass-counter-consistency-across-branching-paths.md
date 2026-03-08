# Review Pattern: Verify FAIL/PASS counter consistency across branching paths

**Review-Area**: testing
**Detection-Hint**: When a loop increments a counter (FAIL) on a condition, and a subsequent if/else block also increments the same counter in the else branch, check whether the else branch can fire after the loop already incremented. Look for `else` branches that catch multiple distinct failure scenarios where one has already been counted.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

In test functions that use a loop to validate items and increment FAIL per bad item, then follow with an if/else summary block: verify the else branch doesn't re-increment FAIL for items already counted in the loop. The else should use `elif [ -z "$var" ]` to only catch the 'no results found' case, not the 'some items failed validation' case.

## Why it matters

Double-counting FAIL inflates the failure count, making test reports inaccurate. It can also mask the true number of distinct failures, undermining trust in the test harness. This is especially insidious because the tests still 'work' — they just report wrong numbers.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: The `else` branch fires for two distinct scenarios: 1. `base_sbom_ifs` is empty — FAIL has not yet been incremented, so incrementing here is correct. 2. `all_guarded=false` — FAIL was **already incremented** inside the while loop (once per step missing the guard). The `else` branch then increments FAIL a second time for the same failure event, overcounting.
- **What was missed**: In test functions that use a loop to validate items and increment FAIL per bad item, then follow with an if/else summary block: verify the else branch doesn't re-increment FAIL for items already counted in the loop. The else should use `elif [ -z "$var" ]` to only catch the 'no results found' case, not the 'some items failed validation' case.
- **Fix**: Changed `else` to `elif [ -z "$base_sbom_ifs" ]` so the extra FAIL increment only fires when no results were found at all, matching the pattern already used in `test_sbom_format_cyclonedx_json` and `test_sbom_attestation_push_to_registry`.
