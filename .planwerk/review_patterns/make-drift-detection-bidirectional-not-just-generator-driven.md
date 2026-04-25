# Review Pattern: Make drift detection bidirectional, not just generator-driven

**Review-Area**: validation
**Detection-Hint**: When reviewing a generator/fixture-drift checker, look for loops that iterate only over the in-code source list (e.g. FIXTURES). Ask: what happens if a file exists on disk that isn't in the list? If the answer is 'silently ignored', it's one-sided drift detection.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Drift/check modes must compare both directions: (1) every declared fixture has a matching file, and (2) every file matching the fixture pattern is declared. Orphan files left after a deletion must surface as drift, not be tolerated.

## Why it matters

One-sided checks let stale or orphaned fixtures survive deletions in the source list, causing tests to silently exercise removed cases or load unintended data — exactly the class of bug the drift check exists to prevent.

## Examples from external reviews

### CC-0094 — berendt
- **Feedback**: _generate.py iterates only over FIXTURES. If a contributor removes an entry from FIXTURES but forgets to `git rm` the corresponding YAML, the orphan file remains on disk and `--check` does not flag it.
- **What was missed**: Drift/check modes must compare both directions: (1) every declared fixture has a matching file, and (2) every file matching the fixture pattern is declared. Orphan files left after a deletion must surface as drift, not be tolerated.
- **Fix**: Added an orphan-file sweep using regex `^(0[2-9]|1[0-9])-.+\.yaml$` after the FIXTURES loop, reporting orphans as drift in `--check` mode and unlinking them otherwise.
