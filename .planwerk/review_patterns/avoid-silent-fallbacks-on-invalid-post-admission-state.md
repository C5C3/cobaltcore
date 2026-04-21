# Review Pattern: Avoid silent fallbacks on invalid post-admission state

**Review-Area**: error-handling
**Detection-Hint**: Look for code paths where a non-nil struct field has an empty required sub-field and the code silently falls back to a default instead of erroring or logging
**Severity**: WARNING
**Occurrences**: 1

## What to check

When spec fields are expected to be validated by admission, unexpected empty sub-values should surface as errors or logs rather than being masked by a fallback

## Why it matters

Silent fallbacks mask misconfiguration and webhook/admission bugs, producing subtly wrong runtime behavior (e.g. wrong URLs) that is hard to diagnose

## Examples from external reviews

### CC-0065 — sourcery-ai[bot]
- **Feedback**: In keystoneStatusEndpoint, when spec.gateway is non-nil but hostname is empty you silently fall back to the cluster-local URL; if this situation should never occur post-admission, it might be safer to log or surface this as a validation/error.
- **What was missed**: When spec fields are expected to be validated by admission, unexpected empty sub-values should surface as errors or logs rather than being masked by a fallback
- **Fix**: Removed the silent hostname fallback in keystoneStatusEndpoint so misconfiguration surfaces loudly
