# Review Pattern: Flag duplicated logic blocks with sync cross-references

**Review-Area**: documentation
**Detection-Hint**: When reviewing a step or block that looks structurally identical to another block in the same file or workflow, diff the two. If they must remain identical due to platform constraints, check for a comment explicitly documenting the coupling.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Identify copy-pasted logic blocks that must stay in sync. Verify either (a) the duplication is eliminated via a shared mechanism, or (b) both blocks have comments cross-referencing each other with a warning about the sync requirement.

## Why it matters

When duplicated logic diverges silently, downstream steps operate on stale assumptions. In this case, smoke-test would attempt to pull an image tag that was never pushed, making the test pass vacuously or fail with a misleading error — and existing tests only validated one of the two copies.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The `Derive image ref` step in `smoke-test` is a verbatim copy of the `Derive tags` step in `build-service-images`. The two blocks will produce the same composite ref as long as they stay in sync, but there is no structural guarantee. If the tag format is changed in `build-service-images` and the corresponding update is missed here, `smoke-test` will try to `docker pull` a ref that was never pushed.
- **What was missed**: Identify copy-pasted logic blocks that must stay in sync. Verify either (a) the duplication is eliminated via a shared mechanism, or (b) both blocks have comments cross-referencing each other with a warning about the sync requirement.
- **Fix**: Added a cross-reference comment above the smoke-test step: `# NOTE: This tag derivation MUST stay in sync with the 'Derive tags' step in build-service-images. If the tag format changes there, update this step too.` Also added test cases covering both instances.
