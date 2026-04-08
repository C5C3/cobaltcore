# Review Pattern: Guard validators against zero-value inputs before parsing

**Review-Area**: validation
**Detection-Hint**: Look for Parse/decode/unmarshal calls in validation functions that operate on string fields without first checking for empty string. Cross-reference with defaulting logic — if the default is applied externally (e.g., CRD schema markers, ORM defaults) rather than in code, the validator can receive zero-value inputs when called outside the normal pipeline.
**Severity**: WARNING
**Occurrences**: 2

## What to check

When a validation function calls a parser (cron, URL, regex, semver, etc.) on a field whose default value is set by an external mechanism (schema defaults, annotations, database defaults) rather than by in-code defaulting, verify the validator handles the zero/empty value explicitly with a clear Required error instead of letting the parser produce a cryptic failure message.

## Why it matters

Validation functions may be called outside the expected pipeline (CLI tools, dry-run helpers, envtest without CRD schema defaulting, unit tests). Without an empty-value guard, users get confusing parser errors ('expected 5 fields, found 0') instead of actionable 'field is required' messages. This also creates a hidden coupling between the validation and the external defaulting mechanism.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: `validate()` unconditionally calls `cron.ParseStandard(k.Spec.Fernet.RotationSchedule)` with no guard for the empty-string case. If `Default()` and `ValidateCreate()` are ever called sequentially *outside* the normal admission chain... a zero-value `Keystone{}` object would fail with `"invalid cron expression: expected exactly 5 fields, found 0"` rather than the more informative "rotationSchedule is required".
- **What was missed**: When a validation function calls a parser (cron, URL, regex, semver, etc.) on a field whose default value is set by an external mechanism (schema defaults, annotations, database defaults) rather than by in-code defaulting, verify the validator handles the zero/empty value explicitly with a clear Required error instead of letting the parser produce a cryptic failure message.
- **Fix**: Added an `if k.Spec.Fernet.RotationSchedule == ""` guard that returns a `field.Required` error with the message 'rotationSchedule must be set; default is "0 0 * * 0"', and moved the `cron.ParseStandard` call into an `else if` branch.

### CC-0040 — sourcery-ai[bot]
- **Feedback**: This logic might incorrectly flip an explicitly set `httpKeepAlive: false` to `true` when `processes`/`threads` are left at zero in scenarios where CRD defaults aren't applied (e.g. some `kubectl patch` paths or weaker schema enforcement). Please confirm the ordering of CRD defaulting vs. this webhook.
- **What was missed**: When defaulting logic uses 'all fields are zero-valued' as a heuristic for 'nothing was set', verify that no legitimate partial-spec scenario (e.g., only HTTPKeepAlive explicitly set to false, with processes/threads left to be defaulted) can reach this code path and have user intent overwritten. Consider the ordering of CRD schema defaults vs. webhook invocation across all mutation paths (create, update, patch).
- **Fix**: Clarified/adjusted the defaulting logic to avoid overriding an explicitly-set HTTPKeepAlive value when processes and threads are still at zero.
