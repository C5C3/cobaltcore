# Review Pattern: Avoid brittle line-number references in comments

**Review-Area**: documentation
**Detection-Hint**: Scan added/changed comments and docs for references like `file.ext:123` or `lines 100-110`; flag any pointer to a specific line number in another file.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Comments, docstrings, and docs should reference stable anchors (function names, step names, section titles) rather than line numbers, which drift as files evolve.

## Why it matters

Line-number references go stale on the next unrelated edit, misleading future readers and creating silent documentation rot.

## Examples from external reviews

### CC-0084 — gndrmnn
- **Feedback**: Remove the specific line reference `611-614` as this will likely be stale in the future
- **What was missed**: Comments, docstrings, and docs should reference stable anchors (function names, step names, section titles) rather than line numbers, which drift as files evolve.
- **Fix**: Replaced `ci.yaml:611-614` with a reference to the named step ("Load images into kind" in `.github/workflows/ci.yaml`).
