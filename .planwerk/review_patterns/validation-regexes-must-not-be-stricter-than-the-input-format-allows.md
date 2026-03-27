# Review Pattern: Validation regexes must not be stricter than the input format allows

**Review-Area**: validation
**Detection-Hint**: When reviewing a regex that validates file content, check the documented format of that content. If the file contains regexes (like stestr exclude patterns), the validator must not reject valid regex metacharacters (^, ., *, etc.) as invalid input.
**Severity**: WARNING
**Occurrences**: 1

## What to check

The grep pattern `'^[0-9]+:(#|[a-zA-Z_])'` rejects lines starting with regex metacharacters like `^`, `.`, or digits. Verify that validation rules match the actual specification of the input format, not an assumed subset.

## Why it matters

Overly restrictive validation causes false positives — legitimate exclude patterns get flagged as errors, forcing contributors to work around the validator or weakening their test exclusions.

## Examples from external reviews

### CC-0034 — sourcery-ai[bot]
- **Feedback**: The regex will flag perfectly valid stestr regex patterns that start with `^`, `.`, digits, or other metacharacters as invalid; consider loosening this to only enforce 'comment or non-empty' semantics.
- **What was missed**: The grep pattern `'^[0-9]+:(#|[a-zA-Z_])'` rejects lines starting with regex metacharacters like `^`, `.`, or digits. Verify that validation rules match the actual specification of the input format, not an assumed subset.
- **Fix**: Change the validation to only reject blank lines (or enforce comment-or-non-empty semantics) instead of constraining the first character of regex pattern lines.
