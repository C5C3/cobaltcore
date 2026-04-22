# Review Pattern: Return deep copies when exposing mutable struct fields

**Review-Area**: architecture
**Detection-Hint**: When a function returns a pointer or slice/map sourced from a caller-supplied struct field, check whether the docstring claims independence and whether DeepCopy is used to back that claim.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For functions that return pointers/slices/maps derived from input struct fields (e.g. spec fields), verify either that a DeepCopy is performed or that the docstring accurately documents the aliasing relationship so callers don't mutate shared state unexpectedly.

## Why it matters

Silent aliasing between returned values and source struct fields leads to action-at-a-distance mutations. Either deep-copying or clearly documenting the alias prevents subtle bugs where a caller mutates what it assumed was an independent value.

## Examples from external reviews

### CC-0084 — berendt
- **Feedback**: deploymentStrategy now returns *keystone.Spec.Strategy.DeepCopy() on the override path and the docstring was rewritten to describe the aliasing guarantee accurately.
- **What was missed**: For functions that return pointers/slices/maps derived from input struct fields (e.g. spec fields), verify either that a DeepCopy is performed or that the docstring accurately documents the aliasing relationship so callers don't mutate shared state unexpectedly.
- **Fix**: Changed deploymentStrategy to return *keystone.Spec.Strategy.DeepCopy() and updated the docstring to accurately describe aliasing.
