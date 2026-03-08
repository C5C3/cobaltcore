# Review Pattern: Check documentation for internal contradictions

**Review-Area**: documentation
**Detection-Hint**: When a bullet or paragraph gives a reason for some behavior, scan the surrounding section for statements that contradict or narrow that reason. Look for causal claims that are only partially true.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Verify that explanations and justifications within the same document are consistent with each other. If paragraph A says X happens because of Y, and paragraph B describes a scenario where Y does not apply but X still happens, the explanation in A is misleading.

## Why it matters

Misleading causal explanations lead readers to wrong mental models. They may assume the behavior would change if the stated cause changes, when in reality the guard is broader than described.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: The parenthetical explanation `(service images are not pushed to GHCR on PRs)` implies the sole reason SBOMs are skipped on PRs is because service images aren't pushed. However, base images **are** always pushed to GHCR on PRs (as the next paragraph correctly states), yet base image SBOM generation is also skipped.
- **What was missed**: Verify that explanations and justifications within the same document are consistent with each other. If paragraph A says X happens because of Y, and paragraph B describes a scenario where Y does not apply but X still happens, the explanation in A is misleading.
- **Fix**: Replaced the misleading parenthetical with accurate wording: the `if: github.event_name != 'pull_request'` guard applies uniformly to all SBOM/attestation steps, including for base images which are pushed on PRs.
