# Review Pattern: Split RBAC rules when verb sets diverge per resource

**Review-Area**: security
**Detection-Hint**: Scan kubebuilder RBAC markers and Helm role/clusterrole templates for rules that combine multiple resources under one verb list; flag when one resource should not have a verb the others need (e.g. delete).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any RBAC rule covering multiple resources with a broad verb list. Confirm every resource in the rule actually requires every verb; split into separate rules when a verb (especially delete/patch/update) only applies to a subset.

## Why it matters

Bundling resources under a superset of verbs grants unnecessary privileges, violating least privilege and expanding blast radius if the service account is compromised.

## Examples from external reviews

### CC-0079 — berendt
- **Feedback**: Split the combined RBAC rule into separate rules for externalsecrets (no delete) and pushsecrets (with delete).
- **What was missed**: Any RBAC rule covering multiple resources with a broad verb list. Confirm every resource in the rule actually requires every verb; split into separate rules when a verb (especially delete/patch/update) only applies to a subset.
- **Fix**: Separated the rule across _helpers.tpl, kubebuilder markers in keystone_controller.go, and Helm test assertions so externalsecrets no longer carry the delete verb.
