# Review Pattern: Flag duplicated logic blocks with sync cross-references

**Review-Area**: documentation
**Detection-Hint**: When reviewing a step or block that looks structurally identical to another block in the same file or workflow, diff the two. If they must remain identical due to platform constraints, check for a comment explicitly documenting the coupling.
**Severity**: WARNING
**Occurrences**: 4

## What to check

Identify copy-pasted logic blocks that must stay in sync. Verify either (a) the duplication is eliminated via a shared mechanism, or (b) both blocks have comments cross-referencing each other with a warning about the sync requirement.

## Why it matters

When duplicated logic diverges silently, downstream steps operate on stale assumptions. In this case, smoke-test would attempt to pull an image tag that was never pushed, making the test pass vacuously or fail with a misleading error — and existing tests only validated one of the two copies.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The `Derive image ref` step in `smoke-test` is a verbatim copy of the `Derive tags` step in `build-service-images`. The two blocks will produce the same composite ref as long as they stay in sync, but there is no structural guarantee. If the tag format is changed in `build-service-images` and the corresponding update is missed here, `smoke-test` will try to `docker pull` a ref that was never pushed.
- **What was missed**: Identify copy-pasted logic blocks that must stay in sync. Verify either (a) the duplication is eliminated via a shared mechanism, or (b) both blocks have comments cross-referencing each other with a warning about the sync requirement.
- **Fix**: Added a cross-reference comment above the smoke-test step: `# NOTE: This tag derivation MUST stay in sync with the 'Derive tags' step in build-service-images. If the tag format changes there, update this step too.` Also added test cases covering both instances.

### CC-0032 — greptile-apps[bot]
- **Feedback**: The DECISION comment explaining the spec deviation (`anchore/grype-action` → `anchore/scan-action`, `fail-on` → `severity-cutoff`) is present only on the `python-base` scan step. The `venv-builder` scan step (~line 183) and the service image scan step (~line 418) do not include this explanation.
- **What was missed**: If a rationale or DECISION comment is added to one instance of a repeated pattern, confirm every other instance either includes the same comment or a cross-reference to it.
- **Fix**: Added '# See DECISION comment on Scan python-base step above' to the venv-builder and service image scan steps.

### CC-0034 — berendt
- **Feedback**: Three steps in `test-service-images` — `Resolve source ref`, `Apply patches`, and `Apply constraint overrides` — are verbatim copies of steps in `build-service-images` with no cross-reference comments. If the source-ref resolution logic in `build-service-images` is updated, the copies in `test-service-images` will silently diverge.
- **What was missed**: Any block of code (3+ lines) that is copy-pasted from another location in the same file or workflow must have a cross-reference comment identifying its counterpart, so future editors know to update both.
- **Fix**: Added `# NOTE: This step MUST stay in sync with the equivalent step in build-service-images.` comments above each of the three duplicated steps in test-service-images.

### CC-0099 — berendt
- **Feedback**: `grep -rln "^feature:" docs/` returns only this file. No other reference doc carries a `feature:` field, no schema or VitePress config consumes it, and there's no precedent for the convention.
- **What was missed**: New frontmatter keys should either follow an existing convention (multiple files use it) or be backed by a documented schema/consumer. A single-file novel field with no consumer is dead metadata or an undocumented convention.
- **Fix**: Removed the `feature:` line from the frontmatter per the reviewer's primary suggestion — CC-0099 is already inline-tagged in the table rows that matter, so no consumer-less convention is introduced.
