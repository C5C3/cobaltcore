# Review Pattern: Capitalize proper tool names consistently

**Review-Area**: documentation
**Detection-Hint**: When proper-noun tool names (Chainsaw, Kubernetes, Helm, etc.) appear in prose, verify capitalization matches how the tool is referred to elsewhere in the same document.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Scan documentation prose for lowercase occurrences of tool names that are capitalized in surrounding text or in the tool's own branding.

## Why it matters

Inconsistent capitalization makes it ambiguous whether the text refers to the specific named tool or a generic concept, and looks unpolished.

## Examples from external reviews

### CC-0094 — sourcery-ai[bot]
- **Feedback**: Consider capitalizing "Chainsaw" here for consistency with how the tool is referenced elsewhere.
- **What was missed**: Scan documentation prose for lowercase occurrences of tool names that are capitalized in surrounding text or in the tool's own branding.
- **Fix**: Changed 'is pinned by a chainsaw step' to 'is pinned by a Chainsaw step' in docs/reference/keystone-crd.md.
