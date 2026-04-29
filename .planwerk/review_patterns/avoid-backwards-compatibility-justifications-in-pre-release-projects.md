# Review Pattern: Avoid backwards-compatibility justifications in pre-release projects

**Review-Area**: documentation
**Detection-Hint**: Scan godoc/comments for phrases like 'for backwards compatibility', 'to preserve compatibility', 'legacy shape' in projects that have not had a stable release.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Comments or docstrings that justify a design choice (pointer shape, field ordering, deprecated names) by appealing to backwards compatibility, when the project has never shipped a stable release and breaking changes are acceptable.

## Why it matters

Such comments encode a false constraint, discourage future cleanup, and mislead readers into thinking the API is frozen when it is still in alpha and freely breakable.

## Examples from external reviews

### CC-0096 — gndrmnn
- **Feedback**: This project has never had a stable release yet. The definitions are fine to break. Nobody uses this productively yet. Remove the comment about backwards compatibility.
- **What was missed**: Comments or docstrings that justify a design choice (pointer shape, field ordering, deprecated names) by appealing to backwards compatibility, when the project has never shipped a stable release and breaking changes are acceptable.
- **Fix**: Removed the trailing sentence about preserving the pointer shape 'for backwards compatibility with envtest fixtures' from the TrustFlush godoc; regenerated CRD YAMLs picked up the same change.
