# Review Pattern: Extract duplicated test assertion blocks into shared helpers

**Review-Area**: testing
**Detection-Hint**: When a PR adds the same assertion block (5+ lines) in more than two test files, flag it for extraction into a shared test helper. Copy-paste across test files is a strong signal.
**Severity**: WARNING
**Occurrences**: 4

## What to check

Are the same security-context assertions (AllowPrivilegeEscalation, ReadOnlyRootFilesystem, RunAsNonRoot, Capabilities, SeccompProfile) copy-pasted across multiple test files? If so, extract a single `assertRestrictedSecurityContext(t, sc)` helper.

## Why it matters

Duplicated assertion logic means that when the restricted security context definition changes, every copy must be updated in lockstep. A missed update silently weakens the test coverage. A single helper keeps the expected profile defined in one place, consistent with the production helper it mirrors.

## Examples from external reviews

### CC-0045 — sourcery-ai[bot]
- **Feedback**: There is a fair amount of duplicated expectation logic across the security context tests (DB sync, bootstrap, fernet, credential); extracting a small helper that asserts a container has the restricted security context would reduce repetition and keep the requirements consistent across tests.
- **What was missed**: Are the same security-context assertions (AllowPrivilegeEscalation, ReadOnlyRootFilesystem, RunAsNonRoot, Capabilities, SeccompProfile) copy-pasted across multiple test files? If so, extract a single `assertRestrictedSecurityContext(t, sc)` helper.
- **Fix**: Create an `assertRestrictedSecurityContext(t *testing.T, sc *corev1.SecurityContext)` test helper and call it from each controller's test instead of repeating the 8-line assertion block.

### CC-0070 — berendt
- **Feedback**: The exact same 2-line fakeRecorder assertion block is copy-pasted 37 times across test files. If the FakeRecorder assertion pattern ever needs to change, every copy must be updated in lockstep.
- **What was missed**: When reviewing test files, look for copy-pasted assertion sequences that cast or unwrap a test double and then assert on it. Count occurrences across all *_test.go files in the package.
- **Fix**: Created shared expectEvent(g, r, substring) and expectNoEvent(g, r) helpers in event_helpers_test.go and replaced all 37 inline assertion blocks across 5 test files.

### CC-0093 — sourcery-ai[bot]
- **Feedback**: The new `bao kv` polling script in `concurrent-cr-conflicts/chainsaw-test.yaml` repeats a lot of setup and error-handling logic that is very similar to the deletion-cleanup chainsaw step; consider factoring the common BAO_ADDR/BAO_TOKEN/bootstrap and polling pattern into a reusable snippet or at least aligning variable names and structure to make future changes less error-prone.
- **What was missed**: New inline scripts in chainsaw-test.yaml files that repeat setup (env vars, bootstrap) and polling/retry patterns already present in sibling steps. Check for a reusable snippet or at least consistent variable naming and structure.
- **Fix**: Align variable names and structure with the sibling deletion-cleanup step (or extract a shared snippet) so bootstrap and polling logic is defined once.

### CC-0094 — sourcery-ai[bot]
- **Feedback**: All invalid-CR manifests repeat essentially the same minimal valid Keystone spec with a single field tweaked; consider factoring the common scaffold out via YAML anchors/aliases or a generator to reduce duplication and the risk of these inputs drifting out of sync with each other over time.
- **What was missed**: Look for multiple test input files that repeat the same minimal valid object with a single field tweaked per file; recommend YAML anchors/aliases or a generator with a drift-check mode.
- **Fix**: Added tests/e2e/keystone/invalid-cr/_generate.py as a single-source-of-truth generator that emits all 10 CC-0094 fixtures from a canonical scaffold and supports a `--check` drift mode for CI.
