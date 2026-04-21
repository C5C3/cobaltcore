# Review Pattern: Detect duplicated parallel resource lists

**Review-Area**: architecture
**Detection-Hint**: Look for two or more slices/arrays that enumerate the same set of resource types or kinds in different phases (e.g., delete vs check, create vs validate).
**Severity**: WARNING
**Occurrences**: 2

## What to check

When a function defines multiple collections referencing the same logical set of types, check whether they are built from a single source of truth. If each list hard-codes the same types independently, flag it as a drift risk.

## Why it matters

Parallel lists drift silently: adding a type to one list but forgetting the other causes subtle bugs (e.g., resources deleted but never verified as gone, or vice versa) that are easy to miss in review and hard to catch in tests.

## Examples from external reviews

### CC-0078 — sourcery-ai[bot]
- **Feedback**: toDelete and toCheck each define the MariaDB resources separately, which risks them drifting (e.g., adding a type to delete but not to check). Please centralize the resource kinds (e.g., via a slice of constructors or a small struct describing the GVK) and build both lists from that shared definition.
- **What was missed**: When a function defines multiple collections referencing the same logical set of types, check whether they are built from a single source of truth. If each list hard-codes the same types independently, flag it as a drift risk.
- **Fix**: Introduced a shared mariaDBResourceCtors slice of constructor functions; both the Delete and Get phases iterate over this single slice instead of maintaining separate hard-coded lists.

### CC-0078 — berendt
- **Feedback**: The hard-coded list of MariaDB CR kinds (Database, User, Grant) exists twice: once in isFirstFinalizingObservation and once in finalizeDatabaseResources. Adding a fourth MariaDB CR kind requires remembering both sites; the sentinel would then silently undercount while the cleanup deletes the new kind — or vice versa.
- **What was missed**: When a set of resource kinds is iterated in one location (e.g. cleanup/deletion), verify that any sibling location checking the same set (e.g. sentinel/existence checks) references a single shared source rather than re-declaring the list.
- **Fix**: Hoist the MariaDB resource constructors to a single package-level var and iterate over it from both the sentinel observation site and the finalizer cleanup site.
