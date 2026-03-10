# Review Pattern: Test defaulting and validation together on zero-value objects

**Review-Area**: testing
**Detection-Hint**: Check whether unit tests exercise the full lifecycle (Default → Validate) on a bare/zero-value object, not just on pre-populated 'valid' fixture objects. If every test helper pre-fills fields that would normally be set by external defaulting, the gap between Default() output and Validate() input is never tested.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When reviewing webhook or admission tests, verify there is at least one test that calls Default() followed by ValidateCreate()/ValidateUpdate() on a minimal or zero-value object — simulating what happens when the object bypasses external schema defaulting. Test helpers that always supply 'happy path' values can mask implicit preconditions.

## Why it matters

Test fixtures that always pre-populate fields hide implicit dependencies between defaulting and validation. When the code is reused outside the expected pipeline (envtest, CLI preflight, reconciler checks), these hidden assumptions surface as confusing runtime errors that the test suite never caught.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The unit tests avoid this by ensuring `validKeystone()` always sets `RotationSchedule` explicitly, but the combination of `Default()` + `ValidateCreate()` on a bare `&Keystone{}` is never exercised together.
- **What was missed**: When reviewing webhook or admission tests, verify there is at least one test that calls Default() followed by ValidateCreate()/ValidateUpdate() on a minimal or zero-value object — simulating what happens when the object bypasses external schema defaulting. Test helpers that always supply 'happy path' values can mask implicit preconditions.
- **Fix**: Added a new unit test `TestValidate_EmptyRotationScheduleReturnsRequiredError` that exercises the empty-string path directly.
