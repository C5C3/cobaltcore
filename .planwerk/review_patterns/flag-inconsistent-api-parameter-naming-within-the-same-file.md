# Review Pattern: Flag inconsistent API parameter naming within the same file

**Review-Area**: naming
**Detection-Hint**: When the same script calls similar APIs (e.g., auth role writes), scan for parameter name inconsistencies. If one call uses `token_policies`/`token_ttl` and another uses `policies`/`ttl`, flag it.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Kubernetes auth role writes at lines 61-67 use deprecated `policies` and `ttl` parameter names, while the AppRole role write at line 77 uses the canonical `token_policies` and `token_ttl`. All role writes in the same script should use the same (canonical) parameter names.

## Why it matters

Inconsistency within the same file confuses operators and hides deprecated usage. The deprecated aliases (`policies`, `ttl`) may be removed in a future OpenBao release, causing a bootstrap failure that would be hard to diagnose.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: The Kubernetes auth role writes use the deprecated `policies` and `ttl` field names. The AppRole role write at line 77 correctly uses the current `token_policies` and `token_ttl` names... This is an inconsistency within the same script.
- **What was missed**: Kubernetes auth role writes at lines 61-67 use deprecated `policies` and `ttl` parameter names, while the AppRole role write at line 77 uses the canonical `token_policies` and `token_ttl`. All role writes in the same script should use the same (canonical) parameter names.
- **Fix**: Updated all four Kubernetes auth role writes from `policies`/`ttl` to `token_policies`/`token_ttl` to match the AppRole style.
