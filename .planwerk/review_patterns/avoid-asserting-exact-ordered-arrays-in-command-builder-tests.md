# Review Pattern: Avoid asserting exact ordered arrays in command-builder tests

**Review-Area**: testing
**Detection-Hint**: When reviewing tests for functions that build CLI argument lists, check whether the assertions verify the full ordered slice via deep-equal. If so, flag as brittle: adding, removing, or reordering any unrelated flag will break every test case.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Unit tests for command/argument builders should assert on the presence of required flag/value pairs and absence of excluded flags, not on the exact ordered array. Use contains/subset assertions or a helper that checks key-value pairs regardless of position.

## Why it matters

Full-array assertions couple every test to the exact ordering of all flags. Any future change that adds or reorders a flag breaks all existing tests, creating unnecessary churn and discouraging incremental improvements to the command builder.

## Examples from external reviews

### CC-0040 — sourcery-ai[bot]
- **Feedback**: The `uwsgiCommand` unit tests assert the full ordered command array, which makes them brittle to unrelated flag ordering changes; you could make them more resilient by asserting on required flag/value pairs and presence/absence of `--http-keepalive` rather than the complete sequence.
- **What was missed**: Unit tests for command/argument builders should assert on the presence of required flag/value pairs and absence of excluded flags, not on the exact ordered array. Use contains/subset assertions or a helper that checks key-value pairs regardless of position.
- **Fix**: Refactored tests to assert that the result contains specific flag/value pairs (e.g., '--processes', '4') and conditionally contains or omits '--http-keepalive', without requiring a specific ordering of the full argument list.
