# Review Pattern: Avoid asserting exact ordered arrays in command-builder tests

**Review-Area**: testing
**Detection-Hint**: When reviewing tests for functions that build CLI argument lists, check whether the assertions verify the full ordered slice via deep-equal. If so, flag as brittle: adding, removing, or reordering any unrelated flag will break every test case.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Unit tests for command/argument builders should assert on the presence of required flag/value pairs and absence of excluded flags, not on the exact ordered array. Use contains/subset assertions or a helper that checks key-value pairs regardless of position.

## Why it matters

Full-array assertions couple every test to the exact ordering of all flags. Any future change that adds or reorders a flag breaks all existing tests, creating unnecessary churn and discouraging incremental improvements to the command builder.

## Examples from external reviews

### CC-0040 — sourcery-ai[bot]
- **Feedback**: The `uwsgiCommand` unit tests assert the full ordered command array, which makes them brittle to unrelated flag ordering changes; you could make them more resilient by asserting on required flag/value pairs and presence/absence of `--http-keepalive` rather than the complete sequence.
- **What was missed**: Unit tests for command/argument builders should assert on the presence of required flag/value pairs and absence of excluded flags, not on the exact ordered array. Use contains/subset assertions or a helper that checks key-value pairs regardless of position.
- **Fix**: Refactored tests to assert that the result contains specific flag/value pairs (e.g., '--processes', '4') and conditionally contains or omits '--http-keepalive', without requiring a specific ordering of the full argument list.

### CC-0054 — sourcery-ai[bot]
- **Feedback**: The new script is very tightly coupled to the exact formatting and ordering in ci.yaml (e.g., full needs: line match, sed-based section extraction), which will make harmless workflow refactors noisy; consider making the assertions more resilient (e.g., matching individual needs entries or using smaller, more focused regexes).
- **What was missed**: Whether test assertions are coupled to incidental formatting details (field order, whitespace, line structure) rather than semantic content. Grep for full-line string matches against structured config files like YAML or JSON.
- **Fix**: Replaced full-line needs assertion with an assert_needs_entry() helper that matches individual dependency entries, tolerating reordering and formatting changes.

### CC-0066 — berendt
- **Feedback**: W-003 and W-004 (range assertions) were fixed by replacing >= and > comparisons with exact-match assertions in both tests/e2e-chaos/operator-pod-kill/chainsaw-test.yaml (availableReplicas: 2) and tests/e2e/keystone/concurrent-cr-conflicts/chainsaw-test.yaml (availableReplicas: 1, 3 occurrences).
- **What was missed**: Test assertions on deterministic values like replica counts, pod counts, or status codes should use exact equality (e.g., `availableReplicas: 2`) rather than range checks (e.g., `availableReplicas >= 2`). A range assertion can silently pass when the system is in an unexpected state.
- **Fix**: Replaced `availableReplicas >= 2` with `availableReplicas: 2` and `availableReplicas > 0` with `availableReplicas: 1` across both test files.
