# Review Pattern: Include correlation fields in operator log entries

**Review-Area**: documentation
**Detection-Hint**: When reviewing log statements in multi-tenant or controller-based code, check whether the log includes enough fields to identify which specific resource or owner triggered the event. A log line that only says 'deleted ConfigMap X' without naming the owning controller or base resource is nearly useless in production debugging.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Log entries for mutation operations (create, update, delete, prune) should include: (1) the resource being acted on, (2) the owner or parent resource name/UID, and (3) any grouping key (like baseName) that ties the operation to a logical set. Check that variables already available in scope are actually passed to the logger.

## Why it matters

In multi-tenant Kubernetes environments, operators manage resources across many namespaces and owners. Without correlation fields, operators debugging an issue must cross-reference logs with kubectl queries to determine which controller triggered a prune — turning a 30-second lookup into minutes of investigation.

## Examples from external reviews

### CC-0077 — sourcery-ai[bot]
- **Feedback**: When logging pruned ConfigMaps in `PruneImmutableConfigMaps`, it could be useful to include additional contextual fields such as `baseName` and the owner's name/UID to make it easier to correlate prune events with specific controllers in multi-tenant environments.
- **What was missed**: Log entries for mutation operations (create, update, delete, prune) should include: (1) the resource being acted on, (2) the owner or parent resource name/UID, and (3) any grouping key (like baseName) that ties the operation to a logical set. Check that variables already available in scope are actually passed to the logger.
- **Fix**: The logger.Info call was enriched with baseName, ownerName, and ownerUID fields.
