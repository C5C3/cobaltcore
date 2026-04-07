# Review Pattern: Extract shared template logic to avoid duplication drift

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new template/config file whose rules or content are intentionally identical to an existing file, check whether the shared content is extracted into a reusable partial (e.g., Helm named templates, shared includes) rather than copy-pasted.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Compare the new file's content against existing files in the same template directory. If large blocks of rules, permissions, or configuration are duplicated verbatim, flag it and ask for extraction into a shared partial.

## Why it matters

Duplicated rule blocks (e.g., RBAC rules in both Role and ClusterRole) inevitably drift when one file is updated but the other is forgotten, causing hard-to-debug permission mismatches between cluster-scoped and namespace-scoped deployments.

## Examples from external reviews

### CC-0043 — sourcery-ai[bot]
- **Feedback**: The `Role` in `templates/role.yaml` deliberately duplicates the `ClusterRole` rules; consider extracting the shared rules into a named template/partial and including it from both files to avoid future drift when RBAC rules change.
- **What was missed**: Compare the new file's content against existing files in the same template directory. If large blocks of rules, permissions, or configuration are duplicated verbatim, flag it and ask for extraction into a shared partial.
- **Fix**: Create a named Helm template (e.g., `define "keystone-operator.rbacRules"`) in `_helpers.tpl` containing the shared RBAC rules, and include it from both `role.yaml` and `clusterrole.yaml`.
