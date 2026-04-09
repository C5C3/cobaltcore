# Review Pattern: Extract duplicated test assertion blocks into shared helpers

**Review-Area**: testing
**Detection-Hint**: When a PR adds the same assertion block (5+ lines) in more than two test files, flag it for extraction into a shared test helper. Copy-paste across test files is a strong signal.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Are the same security-context assertions (AllowPrivilegeEscalation, ReadOnlyRootFilesystem, RunAsNonRoot, Capabilities, SeccompProfile) copy-pasted across multiple test files? If so, extract a single `assertRestrictedSecurityContext(t, sc)` helper.

## Why it matters

Duplicated assertion logic means that when the restricted security context definition changes, every copy must be updated in lockstep. A missed update silently weakens the test coverage. A single helper keeps the expected profile defined in one place, consistent with the production helper it mirrors.

## Examples from external reviews

### CC-0045 — sourcery-ai[bot]
- **Feedback**: There is a fair amount of duplicated expectation logic across the security context tests (DB sync, bootstrap, fernet, credential); extracting a small helper that asserts a container has the restricted security context would reduce repetition and keep the requirements consistent across tests.
- **What was missed**: Are the same security-context assertions (AllowPrivilegeEscalation, ReadOnlyRootFilesystem, RunAsNonRoot, Capabilities, SeccompProfile) copy-pasted across multiple test files? If so, extract a single `assertRestrictedSecurityContext(t, sc)` helper.
- **Fix**: Create an `assertRestrictedSecurityContext(t *testing.T, sc *corev1.SecurityContext)` test helper and call it from each controller's test instead of repeating the 8-line assertion block.
