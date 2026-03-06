# Review Pattern: Cross-reference documentation examples against implementation

**Review-Area**: documentation
**Detection-Hint**: When a PR modifies a Dockerfile, script, or configuration file, search the docs/ directory for code examples that reference or illustrate the same file. Compare the documented pattern against the actual changed code.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When implementation files (Dockerfiles, scripts, configs) are changed, verify that any documentation code snippets depicting those files are updated to match the new implementation.

## Why it matters

Stale documentation examples mislead developers who follow the docs instead of reading the source. This causes incorrect usage patterns to propagate and erodes trust in project documentation.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: The code example here shows the old install pattern — `/tmp/keystone` as a separate positional argument followed by three distinct `"keystone[extra]"` strings. However, the actual `images/keystone/Dockerfile` (line 16) was updated to use the combined PEP 508 form. This documentation example should be updated to match the implementation.
- **What was missed**: When implementation files (Dockerfiles, scripts, configs) are changed, verify that any documentation code snippets depicting those files are updated to match the new implementation.
- **Fix**: Updated the documentation code snippet from the old multi-argument pip install pattern to the combined PEP 508 form `"/tmp/keystone[ldap,memcache_pool,oauth1]"` matching the actual Dockerfile.
