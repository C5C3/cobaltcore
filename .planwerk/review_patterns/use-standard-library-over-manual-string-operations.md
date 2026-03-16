# Review Pattern: Use standard library over manual string operations

**Review-Area**: naming
**Detection-Hint**: Look for manual length-guard + slice patterns (e.g., `len(s) >= N && s[:N] == prefix`) that replicate what a standard library function already does (`strings.HasPrefix`, `strings.Contains`, etc.).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Are there manual string manipulations that could be replaced by standard library calls like strings.HasPrefix, strings.TrimPrefix, strings.Contains?

## Why it matters

Manual slice indices are error-prone: if the prefix constant changes length but the hardcoded index is not updated, the check silently mismatches. Standard library functions are self-documenting and immune to this class of desync bug.

## Examples from external reviews

### CC-0014 — greptile-apps[bot]
- **Feedback**: The length-guard + manual slice approach is harder to read and could silently mismatch if the prefix constant is ever updated out of sync with the slice index.
- **What was missed**: Are there manual string manipulations that could be replaced by standard library calls like strings.HasPrefix, strings.TrimPrefix, strings.Contains?
- **Fix**: Replaced `len(cm.Name) >= N && cm.Name[:N] == "test-keystone-config-"` with `strings.HasPrefix(cm.Name, "test-keystone-config-")` and added the `strings` import.
