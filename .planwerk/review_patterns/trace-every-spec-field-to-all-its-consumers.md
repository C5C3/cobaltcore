# Review Pattern: Trace every spec field to all its consumers

**Review-Area**: validation
**Detection-Hint**: For each field in the CRD spec, grep for all code paths that logically depend on that value. Verify the spec field is used consistently — not the CR metadata name, not a hardcoded default, and not a different spec field that happens to overlap.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a spec field exists (e.g. `spec.database.database`, `spec.fernet.maxActiveKeys`), confirm it is propagated to every resource or configuration that depends on it. Check for mismatches where one code path reads from the spec but another uses `cr.Name` or a compiled-in default for the same logical value.

## Why it matters

A spec field that is accepted but not propagated to all consumers creates silent configuration drift. In the database case, the connection URL targeted a different database than the one actually created, causing Access Denied errors. In the fernet case, the CronJob ignored the configured max keys and silently pruned valid tokens, causing HTTP 401s.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: `buildDatabase` creates the MariaDB CR with `Name: keystone.Name` and no explicit `Spec.Name` field... However, `reconcileConfig` constructs the SQLAlchemy connection URL using `keystone.Spec.Database.Database`. So the connection URL points to database `spec.database.database` while the MariaDB CR and the Grant operate on database `keystone.Name`.
- **What was missed**: When a spec field exists (e.g. `spec.database.database`, `spec.fernet.maxActiveKeys`), confirm it is propagated to every resource or configuration that depends on it. Check for mismatches where one code path reads from the spec but another uses `cr.Name` or a compiled-in default for the same logical value.
- **Fix**: Aligned the MariaDB Database CR spec and Grant to use `keystone.Spec.Database.Database` instead of `keystone.Name`, matching the connection URL.
