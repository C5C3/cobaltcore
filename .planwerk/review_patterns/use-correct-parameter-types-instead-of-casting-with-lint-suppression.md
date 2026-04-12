# Review Pattern: Use correct parameter types instead of casting with lint suppression

**Review-Area**: type-safety
**Detection-Hint**: Look for type casts (e.g., int32(x)) paired with a //nolint comment. If the value is always used as the target type, the parameter or variable should be declared as that type.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a value is immediately cast to a narrower type and assigned, check whether the source variable or parameter could be declared with the target type directly, letting the type system enforce validity instead of a runtime cast plus a suppression comment.

## Why it matters

A cast with a lint suppression pushes range validation from compile time to a comment-based promise. Declaring the correct type eliminates the cast, the suppression, and the class of bugs where an out-of-range value silently truncates.

## Examples from external reviews

### CC-0059 — sourcery-ai[bot]
- **Feedback**: Consider using an int32-typed parameter to avoid the cast and gosec suppression. Because `Status.Replicas` is `int32`, declaring `replicas` as `int32` too would remove the need for both the cast and the `//nolint:gosec` G115 suppression.
- **What was missed**: When a value is immediately cast to a narrower type and assigned, check whether the source variable or parameter could be declared with the target type directly, letting the type system enforce validity instead of a runtime cast plus a suppression comment.
- **Fix**: Changed the `replicas` parameter from `int` to `int32`, removing the `int32(replicas)` cast and the `//nolint:gosec` annotation.
