# Review Pattern: Hardcoded search roots need a completeness guard

**Review-Area**: validation
**Detection-Hint**: When a script hardcodes a list of directories to scan, check whether there is any mechanism to detect new top-level directories that are neither scanned nor explicitly excluded.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Residue/lint scripts with a hardcoded SEARCH_ROOTS list should either derive roots dynamically or validate the hardcoded set against the actual top-level directories, with explicit per-entry exclusion rationale, so newly added directories cannot silently bypass the scan.

## Why it matters

A scan that silently misses new directories gives false confidence — residue can accumulate in a freshly added folder without ever being flagged. The gap is invisible until someone notices the missing coverage months later.

## Examples from external reviews

### CC-0095 — sourcery-ai[bot]
- **Feedback**: The `grep-keystone-api-residue.sh` script hardcodes the set of search roots; if new top-level directories are added later, it would be easy to miss them, so consider deriving the roots from a single shared config or at least validating them against a central list.
- **What was missed**: Residue/lint scripts with a hardcoded SEARCH_ROOTS list should either derive roots dynamically or validate the hardcoded set against the actual top-level directories, with explicit per-entry exclusion rationale, so newly added directories cannot silently bypass the scan.
- **Fix**: Added an EXCLUDED_ROOTS array with per-entry rationale and a completeness check that exits 2 when a new visible top-level directory is neither scanned nor explicitly excluded.
