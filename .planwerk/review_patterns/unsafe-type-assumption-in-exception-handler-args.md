# Review Pattern: Unsafe type assumption in exception handler args

**Review-Area**: error-handling
**Detection-Hint**: In Python except blocks, look for direct use of e.args[0] in comparisons or arithmetic without an explicit type cast. The tuple contents are not guaranteed to be a specific type.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Is e.args[0] compared to an integer literal or used in numeric operations without int() wrapping? DB driver exceptions may populate args[0] with a string representation of the error code depending on the driver version or error path.

## Why it matters

A string-to-int comparison silently evaluates to False in Python, causing the handler to fall through to a generic path or swallow the error entirely, masking the real failure.

## Examples from external reviews

### CC-0064 — berendt
- **Feedback**: Hardened the OperationalError handler with int(e.args[0]) cast.
- **What was missed**: Is e.args[0] compared to an integer literal or used in numeric operations without int() wrapping? DB driver exceptions may populate args[0] with a string representation of the error code depending on the driver version or error path.
- **Fix**: Wrapped e.args[0] with int() cast before comparing against numeric error codes in the OperationalError except block.
