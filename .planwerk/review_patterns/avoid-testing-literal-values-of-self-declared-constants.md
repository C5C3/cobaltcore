# Review Pattern: Avoid testing literal values of self-declared constants

**Review-Area**: testing
**Detection-Hint**: Look for test assertions that compare a constant declared in the same codebase against its literal value (e.g., assert.Equal(t, "some_key", SomeConstant)).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Flag tests that assert the literal value of constants defined in the project. Such tests duplicate the constant declaration without verifying any behavior and create churn whenever the constant is intentionally changed.

## Why it matters

Tests should verify behavior, not tautologically restate source code. Asserting a constant equals its own declared value adds maintenance burden and provides no protection against regressions.

## Examples from external reviews

### CC-0087 — gndrmnn
- **Feedback**: Remove lines 3287-3290. We have no interest in testing the value of constants we declare ourselves. There is no good reason to test if `KeystoneSecretNameIndexKey` has the value we assign to it.
- **What was missed**: Flag tests that assert the literal value of constants defined in the project. Such tests duplicate the constant declaration without verifying any behavior and create churn whenever the constant is intentionally changed.
- **Fix**: Removed the assertion comparing `KeystoneSecretNameIndexKey` to its literal string value, and narrowed the test's docstring to describe the remaining behavior-focused checks (that `registerSecretNameIndex` uses the exported constant and registers against the Keystone type).
