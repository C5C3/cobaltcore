# Review Pattern: Co-locate local path with extras in install commands

**Review-Area**: security
**Detection-Hint**: In Dockerfile or CI install commands, look for a local path and separate package-name-with-extras (e.g., 'pkg[extra]') passed as independent arguments to pip/uv. The extras should reference the local path directly.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When installing a local package with extras, verify the extras are specified on the local path argument itself (e.g., '/path/to/pkg[extra1,extra2]') rather than as separate PyPI-style references alongside the path.

## Why it matters

Separate arguments create ambiguity: if the local source is missing or version-mismatched, the package manager could resolve the extras from PyPI instead of failing, leading to unexpected dependencies or supply-chain risk.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `/tmp/keystone` (local source) and `"keystone[ldap]"` etc. (PyPI references with extras) are passed as separate, independent arguments to `uv pip install`... The single-argument form leaves no room for ambiguity: uv cannot inadvertently resolve `keystone[ldap]` from PyPI if the local source is unexpectedly absent.
- **What was missed**: When installing a local package with extras, verify the extras are specified on the local path argument itself (e.g., '/path/to/pkg[extra1,extra2]') rather than as separate PyPI-style references alongside the path.
- **Fix**: Combined the local path and all extras into a single argument: `"/tmp/keystone[ldap,oauth1]"`.
