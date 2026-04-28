# Review Pattern: Avoid silent fallbacks on invalid post-admission state

**Review-Area**: error-handling
**Detection-Hint**: Look for code paths where a non-nil struct field has an empty required sub-field and the code silently falls back to a default instead of erroring or logging
**Severity**: WARNING
**Occurrences**: 2

## What to check

When spec fields are expected to be validated by admission, unexpected empty sub-values should surface as errors or logs rather than being masked by a fallback

## Why it matters

Silent fallbacks mask misconfiguration and webhook/admission bugs, producing subtly wrong runtime behavior (e.g. wrong URLs) that is hard to diagnose

## Examples from external reviews

### CC-0065 — sourcery-ai[bot]
- **Feedback**: In keystoneStatusEndpoint, when spec.gateway is non-nil but hostname is empty you silently fall back to the cluster-local URL; if this situation should never occur post-admission, it might be safer to log or surface this as a validation/error.
- **What was missed**: When spec fields are expected to be validated by admission, unexpected empty sub-values should surface as errors or logs rather than being masked by a fallback
- **Fix**: Removed the silent hostname fallback in keystoneStatusEndpoint so misconfiguration surfaces loudly

### CC-0089 — berendt
- **Feedback**: The fallback at line 85 silently uses an empty condition_type label... Even the simpler fix — emitting condition_type="UNKNOWN" instead of "" — would be visible in alerts.
- **What was missed**: When a value derived from a map lookup is used as a metric label or log dimension, check what happens on a missing key. Either fail loudly (panic in tests, warn in prod) or substitute a visible sentinel like 'UNKNOWN' instead of allowing an empty string to flow through.
- **Fix**: Added a subReconcilerConditionTypeUnknown sentinel ('UNKNOWN') constant so any unmapped sub_reconciler name surfaces a visible condition_type label instead of an empty string.
