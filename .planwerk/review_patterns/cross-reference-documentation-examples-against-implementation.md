# Review Pattern: Cross-reference documentation examples against implementation

**Review-Area**: documentation
**Detection-Hint**: When a PR modifies a Dockerfile, script, or configuration file, search the docs/ directory for code examples that reference or illustrate the same file. Compare the documented pattern against the actual changed code.
**Severity**: WARNING
**Occurrences**: 4

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

### CC-0043 — berendt
- **Feedback**: The install example uses --set image.tag=latest which contradicts Helm and Kubernetes best practices. latest is a mutable tag that destroys deployment reproducibility and makes rollback ambiguous.
- **What was missed**: All install/upgrade examples in documentation must use pinned, immutable image tags (e.g., v0.1.0) rather than 'latest'. Users copy-paste examples directly; mutable tags destroy reproducibility and make rollback ambiguous.
- **Fix**: Replaced image.tag=latest with image.tag=v0.1.0 in the multi-tenant deployment documentation example.

### CC-0071 — berendt
- **Feedback**: The comment states 'reconcileDatabase (below) depends on their results' which claims a data dependency that does not exist. The PR's own dependency table lists reconcileDatabase's dependencies as [contradicting the comment].
- **What was missed**: Does the comment's claim (e.g. 'X depends on Y's results') match the dependency graph or data flow visible in the code? Do other artifacts in the same PR contradict the comment?
- **Fix**: Correct the comment to accurately reflect the actual dependencies, or remove the dependency claim entirely.
