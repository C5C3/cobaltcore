# Review Pattern: Check resource deletion for orphan cleanup policy

**Review-Area**: validation
**Detection-Hint**: When reviewing code that deletes a parent resource (e.g., a Job, Deployment, or similar orchestrating object), check whether a propagation or cascade policy is specified. A bare delete call without a propagation policy may leave child resources (pods, containers) orphaned.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Find all delete calls for resources that own child resources. Verify each specifies an explicit propagation/cascade policy (e.g., DeletePropagationBackground). If not, flag it.

## Why it matters

Without an explicit propagation policy, deleting a parent resource can leave orphaned child resources running, consuming cluster resources and potentially causing conflicts on re-creation.

## Examples from external reviews

### CC-0058 — berendt
- **Feedback**: Missing PropagationPolicy on deleteValidationJob.
- **What was missed**: Find all delete calls for resources that own child resources. Verify each specifies an explicit propagation/cascade policy (e.g., DeletePropagationBackground). If not, flag it.
- **Fix**: Added DeletePropagationBackground to the deleteValidationJob function (line 203-204) to ensure owned pods are cleaned up when the validation Job is deleted.
