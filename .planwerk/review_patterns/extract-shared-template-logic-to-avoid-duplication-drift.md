# Review Pattern: Extract shared template logic to avoid duplication drift

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new template/config file whose rules or content are intentionally identical to an existing file, check whether the shared content is extracted into a reusable partial (e.g., Helm named templates, shared includes) rather than copy-pasted.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Compare the new file's content against existing files in the same template directory. If large blocks of rules, permissions, or configuration are duplicated verbatim, flag it and ask for extraction into a shared partial.

## Why it matters

Duplicated rule blocks (e.g., RBAC rules in both Role and ClusterRole) inevitably drift when one file is updated but the other is forgotten, causing hard-to-debug permission mismatches between cluster-scoped and namespace-scoped deployments.

## Examples from external reviews

### CC-0043 — sourcery-ai[bot]
- **Feedback**: The `Role` in `templates/role.yaml` deliberately duplicates the `ClusterRole` rules; consider extracting the shared rules into a named template/partial and including it from both files to avoid future drift when RBAC rules change.
- **What was missed**: Compare the new file's content against existing files in the same template directory. If large blocks of rules, permissions, or configuration are duplicated verbatim, flag it and ask for extraction into a shared partial.
- **Fix**: Create a named Helm template (e.g., `define "keystone-operator.rbacRules"`) in `_helpers.tpl` containing the shared RBAC rules, and include it from both `role.yaml` and `clusterrole.yaml`.

### CC-0056 — sourcery-ai[bot]
- **Feedback**: The job construction logic between `buildDBSyncJob` and `buildUpgradeJob` is very similar; consider refactoring shared pieces (container spec, volume mounts, security context) into a common helper to avoid drift when these need to change in the future.
- **What was missed**: Compare all functions that construct the same resource kind (e.g. batchv1.Job). If two or more share >50% of their struct fields verbatim, a shared helper should be extracted.
- **Fix**: Extracted a shared `buildDBJob` helper that both `buildDBSyncJob` and `buildUpgradeJob` delegate to, eliminating duplicated container spec, volume mounts, and security context.
