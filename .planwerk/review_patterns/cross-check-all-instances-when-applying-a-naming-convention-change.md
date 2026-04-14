# Review Pattern: Cross-check all instances when applying a naming convention change

**Review-Area**: documentation
**Detection-Hint**: When a PR updates naming from hardcoded values to a parameterized pattern (e.g., keystone-* to {name}-*), search the entire changeset and related documentation for sibling entries that follow the old convention but were not updated.
**Severity**: WARNING
**Occurrences**: 1

## What to check

If a PR renames N resources from a hardcoded pattern to a parameterized one, verify that ALL sibling resources in the same table/list/config are also updated, not just the ones directly touched by the feature.

## Why it matters

Partial renaming creates inconsistency in documentation and configuration that misleads operators and erodes trust in reference material. It signals the change was applied mechanically to touched files without a full sweep.

## Examples from external reviews

### CC-0073 — berendt
- **Feedback**: The PR correctly updates Fernet and Credential resources from hardcoded keystone-* to {name}-* in the owner resources table, but three sibling resources were left with hardcoded names.
- **What was missed**: If a PR renames N resources from a hardcoded pattern to a parameterized one, verify that ALL sibling resources in the same table/list/config are also updated, not just the ones directly touched by the feature.
- **Fix**: Acknowledged as pre-existing with HTML TODO comments added on the four affected rows in keystone-reconciler.md.
