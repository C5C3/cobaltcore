# Review Pattern: Verify all container specs use the shared security helper introduced in the same PR

**Review-Area**: security
**Detection-Hint**: When a PR introduces a security helper (like restrictedSecurityContext()), grep for all Container struct literals in the changed files and verify each one calls the helper. Any container missing SecurityContext is a gap.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every container spec in the PR's scope has a SecurityContext assigned. Check both new container definitions and pre-existing ones touched by the PR. A helper that exists but isn't applied everywhere creates a false sense of compliance.

## Why it matters

A container without a SecurityContext inherits permissive defaults (root user, full capabilities), violating the restricted profile the PR was meant to enforce. The whole point of extracting the helper is uniform application — missing even one container undermines it.

## Examples from external reviews

### CC-0045 — berendt
- **Feedback**: The Deployment container has no SecurityContext. The PR description claims all workloads are hardened, but reconcile_deployment.go:160-217 builds a container without calling the new restrictedSecurityContext helper.
- **What was missed**: Every container spec in the PR's scope has a SecurityContext assigned. Check both new container definitions and pre-existing ones touched by the PR. A helper that exists but isn't applied everywhere creates a false sense of compliance.
- **Fix**: Added `SecurityContext: restrictedSecurityContext()` to the Deployment container spec in reconcile_deployment.go.
