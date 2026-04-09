# Review Pattern: Move cross-cutting helpers to dedicated files

**Review-Area**: architecture
**Detection-Hint**: When a new helper function is added, check whether it is called from files other than the one it lives in. If callers span multiple controllers or domains, the function belongs in a shared/neutral file.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Is the helper defined in a domain-specific file (e.g. reconcile_deployment.go) but imported/called by other domain files (reconcile_dbsync.go, reconcile_bootstrap.go, etc.)? If yes, it should be extracted to a neutral file.

## Why it matters

Placing a shared utility inside a domain-specific file creates misleading coupling: readers assume the helper is deployment-specific, and future maintainers may hesitate to modify it without understanding all call sites. It also makes the dependency graph harder to reason about.

## Examples from external reviews

### CC-0045 — sourcery-ai[bot]
- **Feedback**: The `restrictedSecurityContext` helper is defined in `reconcile_deployment.go` but used by multiple controllers (database, bootstrap, fernet, credential); consider moving it to a dedicated, neutral file (e.g. `security_context.go`).
- **What was missed**: Is the helper defined in a domain-specific file (e.g. reconcile_deployment.go) but imported/called by other domain files (reconcile_dbsync.go, reconcile_bootstrap.go, etc.)? If yes, it should be extracted to a neutral file.
- **Fix**: Move `restrictedSecurityContext()` from `reconcile_deployment.go` to a new `security_context.go` file in the same package, so its location reflects its cross-cutting scope.
