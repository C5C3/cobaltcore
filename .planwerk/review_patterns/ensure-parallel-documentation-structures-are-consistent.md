# Review Pattern: Ensure parallel documentation structures are consistent

**Review-Area**: documentation
**Detection-Hint**: When a document contains multiple tables or sections describing analogous concepts (e.g., two auth method tables), compare their column structures side by side. Mismatched columns between parallel tables signal a missing detail in one of them.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Identify sibling tables or sections that document similar entities (e.g., Kubernetes auth roles vs. AppRole auth roles). Verify they use the same set of columns when the underlying concepts share the same parameters.

## Why it matters

Inconsistent structure between parallel sections misleads readers into thinking the missing information does not apply, when in reality it was simply omitted. It also makes the document harder to scan and compare.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: Compare with the AppRole table immediately below, which correctly documents both the initial and maximum lifetimes.
- **What was missed**: Identify sibling tables or sections that document similar entities (e.g., Kubernetes auth roles vs. AppRole auth roles). Verify they use the same set of columns when the underlying concepts share the same parameters.
- **Fix**: Aligned the Kubernetes auth role table columns with the AppRole table by adding the missing `Max TTL` column.
