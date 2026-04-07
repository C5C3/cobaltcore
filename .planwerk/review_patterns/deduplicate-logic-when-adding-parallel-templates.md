# Review Pattern: Deduplicate logic when adding parallel templates

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new template that is a conditional alternative to an existing one (e.g., Role alongside ClusterRole, Ingress alongside Route), diff the two files. If they share identical rule/spec blocks, flag the duplication and suggest extracting a shared named template in _helpers.tpl.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check whether newly added templates duplicate rule lists, spec blocks, or other structured content from existing templates. If two templates differ only in kind/scope but share the same body, the shared portion should be a named template.

## Why it matters

Duplicated RBAC rules or spec blocks across templates inevitably diverge over time — a permission added to ClusterRole but missed in Role creates a silent runtime failure that only affects namespace-scoped deployments, which are typically the less-tested path.

## Examples from external reviews

### CC-0043 — berendt
- **Feedback**: RBAC rules were duplicated between clusterrole.yaml and role.yaml with no shared template or cross-reference.
- **What was missed**: Check whether newly added templates duplicate rule lists, spec blocks, or other structured content from existing templates. If two templates differ only in kind/scope but share the same body, the shared portion should be a named template.
- **Fix**: Extracted RBAC rules into a shared named template 'keystone-operator.rbacRules' in _helpers.tpl, included by both clusterrole.yaml and role.yaml.
