# Review Pattern: Cross-reference documentation examples against implementation

**Review-Area**: documentation
**Detection-Hint**: When a PR modifies a Dockerfile, script, or configuration file, search the docs/ directory for code examples that reference or illustrate the same file. Compare the documented pattern against the actual changed code.
**Severity**: WARNING
**Occurrences**: 2

## What to check

When implementation files (Dockerfiles, scripts, configs) are changed, verify that any documentation code snippets depicting those files are updated to match the new implementation.

## Why it matters

Stale documentation examples mislead developers who follow the docs instead of reading the source. This causes incorrect usage patterns to propagate and erodes trust in project documentation.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: The code example here shows the old install pattern — `/tmp/keystone` as a separate positional argument followed by three distinct `"keystone[extra]"` strings. However, the actual `images/keystone/Dockerfile` (line 16) was updated to use the combined PEP 508 form. This documentation example should be updated to match the implementation.
- **What was missed**: When implementation files (Dockerfiles, scripts, configs) are changed, verify that any documentation code snippets depicting those files are updated to match the new implementation.
- **Fix**: Updated the documentation code snippet from the old multi-argument pip install pattern to the combined PEP 508 form `"/tmp/keystone[ldap,oauth1]"` matching the actual Dockerfile.

### CC-0009 — greptile-apps[bot]
- **Feedback**: The Kubernetes auth role table has a single `TTL` column but `setup-auth.sh` configures both an initial TTL of 1 hour *and* a maximum lifetime of 4 hours on each role. Without a `Max TTL` column, an operator reading this table would believe ESO tokens can be renewed indefinitely beyond the initial lifetime — which is incorrect.
- **What was missed**: For each table documenting infrastructure configuration, confirm that every parameter set in the referenced script or config file has a corresponding column. Diff the set of fields between the script and the table.
- **Fix**: Added a `Max TTL` column with value `4h` to the Kubernetes auth role table to match the `setup-auth.sh` implementation.
