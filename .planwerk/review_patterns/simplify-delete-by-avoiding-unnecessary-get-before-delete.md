# Review Pattern: Simplify delete by avoiding unnecessary Get-before-Delete

**Review-Area**: performance
**Detection-Hint**: When a delete function fetches an object only to pass it to `Delete`, check whether the full object is needed. If only name/namespace are required and the function already handles not-found, it can construct a minimal object and use `client.IgnoreNotFound` on the delete error instead.
**Severity**: WARNING
**Occurrences**: 1

## What to check

In `Delete*` helper functions, check if the preceding `Get` call serves any purpose beyond confirming existence. If the fetched object's fields are not inspected (e.g., for finalizers or status), the Get is a wasted API call — construct the object with name/namespace and delete directly.

## Why it matters

An unnecessary Get doubles the API calls for every delete operation. In reconciliation loops that run frequently, this adds avoidable load on the API server and increases reconcile latency.

## Examples from external reviews

### CC-0038 — sourcery-ai[bot]
- **Feedback**: In `DeleteHPA`, you can simplify and avoid the extra `Get` roundtrip by constructing an `HorizontalPodAutoscaler` with just name/namespace and calling `Delete` with `client.IgnoreNotFound(err)` instead of fetching the object first.
- **What was missed**: In `Delete*` helper functions, check if the preceding `Get` call serves any purpose beyond confirming existence. If the fetched object's fields are not inspected (e.g., for finalizers or status), the Get is a wasted API call — construct the object with name/namespace and delete directly.
- **Fix**: Replaced the Get+conditional-Delete pattern with a direct Delete on a minimal object, using `client.IgnoreNotFound(err)` to handle the not-found case.
