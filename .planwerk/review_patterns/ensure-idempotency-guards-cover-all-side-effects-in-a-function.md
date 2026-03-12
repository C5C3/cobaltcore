# Review Pattern: Ensure idempotency guards cover all side effects in a function

**Review-Area**: validation
**Detection-Hint**: When a function claims idempotency via an early existence check, trace all code paths after the check. Look for operations (API calls, mutations, writes) that execute regardless of whether the resource already existed.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For each function documented or designed as idempotent, verify that the guard (e.g., existence check with early return) protects ALL mutating operations, not just the primary create/enable call. Check that secondary operations like tune, configure, or update are also guarded.

## Why it matters

Partially-guarded idempotency is inconsistent with documented behavior and creates unnecessary API calls on re-runs. It erodes trust in the idempotency contract and can cause unexpected failures if the unguarded operation has different preconditions.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `bao secrets tune` runs unconditionally — breaks the declared idempotency of `setup-secret-engines.sh`. The `enable_kv_v2` function correctly guards the `secrets enable` call with an existence check, but `enable_pki` always calls `bao secrets tune` regardless.
- **What was missed**: For each function documented or designed as idempotent, verify that the guard (e.g., existence check with early return) protects ALL mutating operations, not just the primary create/enable call. Check that secondary operations like tune, configure, or update are also guarded.
- **Fix**: Restructured `enable_pki` to early-return when the engine already exists, so `bao secrets tune` only runs on fresh enablement.
