# Review Pattern: Check shell variable quoting in dynamically constructed commands

**Review-Area**: security
**Detection-Hint**: Look for variables expanded inside `sh -c` strings or `eval` without quoting. Compare quoting discipline across branches within the same function — if one branch quotes and another doesn't, flag the inconsistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When building shell command strings, every variable interpolation that could contain user/caller-supplied values must be quoted. In this case, the `@generate` branch properly quotes values inside the `sh -c` command, but the non-generate branch appends `${arg}` unquoted, making it vulnerable to word splitting and shell metacharacter issues.

## Why it matters

Unquoted expansion in `sh -c` causes silent misparsing or command injection if values contain spaces, equals signs, or shell metacharacters. Even if current callers are safe, this is a latent defect that breaks when new callers are added.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: When a key-value argument does NOT use the `@generate` marker, it is appended to `put_args` verbatim without quoting... It would be safer to quote non-generated values with single quotes when building `put_args`.
- **What was missed**: When building shell command strings, every variable interpolation that could contain user/caller-supplied values must be quoted. In this case, the `@generate` branch properly quotes values inside the `sh -c` command, but the non-generate branch appends `${arg}` unquoted, making it vulnerable to word splitting and shell metacharacter issues.
- **Fix**: Non-generated values are now single-quoted when appended: `put_args+=" ${key}='${val}'"` to prevent shell metacharacter issues.
