# Review Pattern: Chainsaw assertions compare equality, not truthiness

**Review-Area**: testing
**Detection-Hint**: In a Chainsaw `assert:` block, flag any `(<jmespath>): <literal>` line where the JMESPath expression returns a non-boolean (string, number, object, array) but the expected literal is `true`/`false`. This is a type mismatch dressed up as a truthiness check.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Chainsaw's `(<jmespath>): <expected>` assertion evaluates the JMESPath expression and performs **equality comparison** against the expected literal. It does not implement JMESPath truthiness (the "falsy if empty" rule from the JMESPath community spec). A string value compared against the bool `true` fails at runtime with `types are not comparable, bool - string`.

To assert "field is present and non-empty", the expression itself must already be boolean. Two reliable idioms in this repo:

- Two-part explicit check: `(field != null && field != ''): true`
- Length-based check: ``(length(not_null(field, '')) > `0`): true``

Both reject missing fields (`null`) and empty strings without hard-coding a literal value that could drift across upstream releases. The existing ``(availableReplicas > `0`): true`` and ``(readyReplicas >= `1`): true`` patterns elsewhere in `tests/e2e/` are the same shape — expression evaluates to bool, expected literal is bool.

## Why it matters

A wrong assertion doesn't just fail the current CI run — if the expression evaluates to bool in some states and non-bool in others (e.g. the field is populated late during reconciliation), the assertion can flip between silent-pass and hard-error depending on timing, making the test's behavior unclear. The JMESPath truthiness story is also one of the most common mis-ports from other assertion libraries (Jest, Mocha, Go's `require.NotEmpty`), so reviewers need to call it out explicitly rather than trusting the author's framing.

## Examples from external reviews

### CC-0085 — berendt
- **Feedback**: External review W-001 recommended replacing `(entitlement != ''): true` with `(entitlement): true` in `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml`, arguing that "JMESPath truthiness" would correctly fail on null, empty string, false, and empty array.
- **What was missed**: Chainsaw compares the JMESPath result against the expected literal for equality — it does not apply JMESPath's truthiness rule. `(entitlement): true` made CI fail with `spec.distribution.(entitlement): Internal error: types are not comparable, bool - string` because `entitlement` is a string (e.g. `"Issued"`). The previously-accepted form `(entitlement != null && entitlement != ''): true` evaluates to bool and correctly rejects both null and empty-string.
- **Fix**: Restored `(entitlement != null && entitlement != ''): true` and rewrote the inline comment to explain Chainsaw's equality semantics so the decision is not reverted again.
