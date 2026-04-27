# Review Pattern: Self-referential script must exclude itself from its own scan

**Review-Area**: validation
**Detection-Hint**: When a script greps for a pattern across the repo, check whether the script itself contains that pattern in comments/headers/variables and whether it excludes itself from the scan.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any grep/lint/residue script that searches for literal or templated patterns should exclude its own filename from the search, or annotate its own references, so it does not self-trip and fail CI on a clean tree.

## Why it matters

A self-tripping CI gate fails on every clean tree, blocking merges and eroding trust in the check. The bug is invisible until CI runs, but trivially preventable at review time.

## Examples from external reviews

### CC-0095 — sourcery-ai[bot]
- **Feedback**: Because this script is included in the literal scan and its header has unannotated `keystone-api` references, it will always report its own header as residue and exit 1. Please either (a) exclude this script from the literal grep (as you already do for the templated scan), or (b) annotate all literal `keystone-api` mentions here with `CC-0095 legacy`.
- **What was missed**: Any grep/lint/residue script that searches for literal or templated patterns should exclude its own filename from the search, or annotate its own references, so it does not self-trip and fail CI on a clean tree.
- **Fix**: Added the script's own filename to the --exclude list for the literal grep scan, matching the existing exclusion already in place for the templated scan.
