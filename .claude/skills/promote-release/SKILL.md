---
name: promote-release
description: >-
  Promote the latest validated CobaltCore release candidate to stable by tagging
  its exact build. Use when a maintainer requests stable promotion.
---

# Promote a CobaltCore release

Fetch the selected repository's distribution tags and inspect its prereleases.
Select the highest `vX.Y.Z-rc.N` by numeric major, minor, patch, and RC number.
For example, `v1.5.0-rc.10` follows `v1.5.0-rc.9` and promotes to `v1.5.0`.
If no RC exists, report that and point to [[prerelease-cobaltcore]].

1. Resolve the RC tag to its commit, peeling annotated tags. Check that the
   published candidate and its validation evidence identify that build and
   that the maintainer has approved it for stable. If the latest RC is still
   awaiting validation, report what is pending.
2. Strip the RC suffix to get the stable tag. Check local and remote tags.
   If the stable tag exists at the same commit, reuse it. If it points
   elsewhere, report both SHAs and stop. Preserve the original RC tag.
3. Create the missing annotated stable tag directly at the RC commit and name
   the promoted RC in the annotation. The release branch may have advanced.
4. Report the RC, stable tag and commit. Leave the tag local unless the
   maintainer requests publication. When publishing, recheck the remote RC
   and stable refs, push only the stable tag to the selected repository, and
   verify that the GitHub Release has stable chart archives, checksums, and
   the same `IMAGE_DIGESTS.json` as the prerelease.
   Pull an operator chart from GHCR with `--version X.Y.Z` and verify its operator image
   defaults to the same stable version. Return the release URL.

[Release management RFC](https://github.com/C5C3/cobaltcore/pull/1645)
