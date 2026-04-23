# Review Pattern: Do not swallow errors by returning zero-values with nil error

**Review-Area**: error-handling
**Detection-Hint**: Look for error-handling branches that discard a non-nil error and return a zero-value (empty string, nil, 0) paired with a nil error. Especially watch for IsNotFound or similar sentinel-error checks that silently convert a meaningful error into a success path.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

When a function catches a specific error class (e.g., IsNotFound) and returns a default value with nil error, verify that (1) the caller truly cannot distinguish this from a real success, and (2) no useful signal is lost. If the caller would benefit from knowing the error occurred, propagate it and let the caller decide.

## Why it matters

Returning a zero-value with nil error hides failure from callers, making the success path indistinguishable from the error path. This prevents callers from implementing appropriate error-specific logic (e.g., requeueing vs. continuing) and can mask real issues in production.

## Examples from external reviews

### CC-0015 — gndrmnn
- **Feedback**: Returning an empty string for the hash and indicating no error does not seem to make much sense in this specific case. It is better for this function to return the is-not-found error to the caller.
- **What was missed**: When a function catches a specific error class (e.g., IsNotFound) and returns a default value with nil error, verify that (1) the caller truly cannot distinguish this from a real success, and (2) no useful signal is lost. If the caller would benefit from knowing the error occurred, propagate it and let the caller decide.
- **Fix**: Removed the IsNotFound early-return inside fernetKeysHash so all errors are propagated. The caller reconcileDeployment was updated to tolerate not-found errors specifically (continuing with an empty hash) while failing on unexpected errors.

### CC-0087 — sourcery-ai[bot]
- **Feedback**: Silently discarding keystonePaths error makes envtest startup failures harder to diagnose. The blank identifier ignores errors from `keystonePaths()`. If it fails ... envtest will later error in a less obvious way.
- **What was missed**: Functions that return an error should have that error captured and handled (t.Fatalf in tests, returned or logged in production code), not assigned to `_`.
- **Fix**: Capture the error and call t.Fatalf with a clear message when keystonePaths fails, matching the other helpers in the file.
