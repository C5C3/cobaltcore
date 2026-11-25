---
name: prerelease-cobaltcore
description: Prepare a CobaltCore release branch and dispatch a semantic-release candidate.
---

# Cut a CobaltCore candidate

Fetch the selected repository's branches and tags. For the first series, cut
`release/0.1` from the selected main commit. For a later series, use
`release/X.Y` containing the previous stable tag and the changes to release.
When backports have diverged from main, include the previous stable commit
in the new series' ancestry. Inspect the full landed commit messages when
choosing the series. A fix or performance improvement increments patch, a
feature increments minor and a breaking-change footer increments major.

If the latest candidate already identifies the selected commit, report its URL.
When a fix is needed, use `cherrypick-into-release` and wait for its PR to merge.
A documentation-only backport ordinarily produces no new candidate.

When the maintainer requests publication, publish the selected series branch
if needed, then run the candidate workflow in that repository:

```bash
gh workflow run ci.yaml --ref release/0.1 -f distribution=candidate
```

Follow the run. Report the prerelease URL, version and SHA. Semantic-release
calculates and creates the tag after the signed artifacts are ready. If an
upload failed, rerun the failed jobs of the same run to reuse its saved plan.
