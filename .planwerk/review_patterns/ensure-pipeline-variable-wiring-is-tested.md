# Review Pattern: Ensure pipeline variable wiring is tested

**Review-Area**: testing
**Detection-Hint**: When a workflow step introduces or consumes a variable (env, output, input), check whether the test suite asserts both that the producing step sets it correctly and that the consuming step references the correct source expression.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For each new variable or parameter introduced in a workflow/pipeline, verify that tests assert: (1) the producing step sets it correctly, and (2) the consuming step references the correct source expression. Missing either side leaves the wiring untested.

## Why it matters

A missing or mistyped variable reference causes silent misconfiguration at runtime — e.g., an empty `INSTALL_SPEC` leads to a `uv pip install` that skips the service package, producing confusing import errors instead of a clear configuration failure.

## Examples from external reviews

### CC-0034 — berendt
- **Feedback**: Test gap — `INSTALL_SPEC` wiring not validated.
- **What was missed**: For each new variable or parameter introduced in a workflow/pipeline, verify that tests assert: (1) the producing step sets it correctly, and (2) the consuming step references the correct source expression. Missing either side leaves the wiring untested.
- **Fix**: Added INSTALL_SPEC wiring validation to the existing test function, asserting that the Run tests step's INSTALL_SPEC env references `steps.pip-extras.outputs.install_spec`, plus a check that the Resolve pip extras step includes `[test]` extra.
