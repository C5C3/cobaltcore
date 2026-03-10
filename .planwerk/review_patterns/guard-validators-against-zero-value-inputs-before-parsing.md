# Review Pattern: Guard validators against zero-value inputs before parsing

**Review-Area**: validation
**Detection-Hint**: Look for Parse/decode/unmarshal calls in validation functions that operate on string fields without first checking for empty string. Cross-reference with defaulting logic — if the default is applied externally (e.g., CRD schema markers, ORM defaults) rather than in code, the validator can receive zero-value inputs when called outside the normal pipeline.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a validation function calls a parser (cron, URL, regex, semver, etc.) on a field whose default value is set by an external mechanism (schema defaults, annotations, database defaults) rather than by in-code defaulting, verify the validator handles the zero/empty value explicitly with a clear Required error instead of letting the parser produce a cryptic failure message.

## Why it matters

Validation functions may be called outside the expected pipeline (CLI tools, dry-run helpers, envtest without CRD schema defaulting, unit tests). Without an empty-value guard, users get confusing parser errors ('expected 5 fields, found 0') instead of actionable 'field is required' messages. This also creates a hidden coupling between the validation and the external defaulting mechanism.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: `validate()` unconditionally calls `cron.ParseStandard(k.Spec.Fernet.RotationSchedule)` with no guard for the empty-string case. If `Default()` and `ValidateCreate()` are ever called sequentially *outside* the normal admission chain... a zero-value `Keystone{}` object would fail with `"invalid cron expression: expected exactly 5 fields, found 0"` rather than the more informative "rotationSchedule is required".
- **What was missed**: When a validation function calls a parser (cron, URL, regex, semver, etc.) on a field whose default value is set by an external mechanism (schema defaults, annotations, database defaults) rather than by in-code defaulting, verify the validator handles the zero/empty value explicitly with a clear Required error instead of letting the parser produce a cryptic failure message.
- **Fix**: Added an `if k.Spec.Fernet.RotationSchedule == ""` guard that returns a `field.Required` error with the message 'rotationSchedule must be set; default is "0 0 * * 0"', and moved the `cron.ParseStandard` call into an `else if` branch.
