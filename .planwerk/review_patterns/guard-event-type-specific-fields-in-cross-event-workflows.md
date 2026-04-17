# Review Pattern: Guard event-type-specific fields in cross-event workflows

**Review-Area**: validation
**Detection-Hint**: When a workflow `if:` condition accesses `github.event.pull_request.*` (labels, head, base, etc.), check whether the workflow also triggers on non-PR events (push, schedule, workflow_dispatch). If so, the access must be guarded with `github.event_name == 'pull_request'` to avoid relying on implicit null coercion.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any workflow `if:` condition that accesses `github.event.pull_request.*` should be guarded with `github.event_name == 'pull_request'` when the workflow also triggers on push or other non-PR events. Without the guard, the expression relies on GitHub Actions' implicit null handling, which is fragile and non-obvious.

## Why it matters

On push events, `github.event.pull_request` is undefined. While GitHub Actions currently treats undefined property access as falsy (empty string), this is implicit behavior that may not be obvious to maintainers and could change. An explicit event-type guard makes the intent clear and prevents subtle breakage if the platform's null-coercion semantics ever change.

## Examples from external reviews

### CC-0049 — sourcery-ai[bot]
- **Feedback**: The e2e-chaos workflow condition references `github.event.pull_request.labels` directly; to make the job more robust across event types (e.g. push vs PR), consider guarding the label check with an `github.event_name == 'pull_request'` predicate to avoid relying on the field's implicit null handling.
- **What was missed**: Any workflow `if:` condition that accesses `github.event.pull_request.*` should be guarded with `github.event_name == 'pull_request'` to avoid relying on implicit null coercion when the workflow also triggers on push or other events.
- **Fix**: Added a `github.event_name == 'pull_request'` predicate to the workflow condition wrapping the label check.
