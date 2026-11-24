---
name: prerelease-cobaltcore
description: >-
  Cut or continue a CobaltCore distribution release branch and tag its next
  release candidate. Use when a maintainer asks to prepare a CobaltCore prerelease.
---

# Prerelease CobaltCore

One RC tag identifies the entire distribution. CI uses the candidate version
for every operator image and chart and for the `target-cluster-access` chart. [RFC 0001](https://github.com/C5C3/cobaltcore/pull/1645)

## Select the series

Identify the target repository and fetch its current `main`, release branches,
and tags. Check the GitHub releases and their corresponding tags. Include RCs
already tagged locally so a repeated invocation sees them. For version selection, use only distribution tags of the form
`vX.Y.Z` or `vX.Y.Z-rc.N`. Compare versions using SemVer, including the
numeric RC number. GitHub's “Latest” release can omit prereleases.

| Highest distribution version | Branch and candidate |
| --- | --- |
| Stable, e.g. `v1.4.3` | Increment the minor version, reset patch to zero, and cut `release/1.5` from the fetched `main` tip. Tag `v1.5.0-rc.1`. |
| RC, e.g. `v1.5.0-rc.9` | Switch to the existing `release/1.5` branch. Keep the base version and tag its tip `v1.5.0-rc.10`. This also applies if `v1.4.3` is still the latest stable release. |
| No distribution tags | Cut `release/0.1` from the fetched `main` tip and start with `v0.1.0-rc.1`, the RFC's initial version. |

If a new series branch already exists from an interrupted run, resume it at
its current tip after verifying its history. For an existing RC, verify its
tagged commit is on the series branch. Fast-forward a local branch that is
behind its remote. Report a missing RC branch, divergent history, or edits
that would be disturbed by switching branches.

## Check the commit before tagging

1. On the selected release branch, resolve its tip to a full commit SHA. Find
   **all tags** pointing to that commit, including annotated tags after peeling
   them to their commits. This check applies to the first RC as well as every
   subsequent candidate.
2. If any tag already points there, **stop before creating another tag**. Show
   the branch, SHA, and existing tags. Prompt the maintainer to cherry-pick
   the intended fixes, for example, “`release/1.5` is still at
   `v1.5.0-rc.9`. Which commits from `main` should be cherry-picked before
   cutting RC 10?” List eligible commits with their SHAs and subjects when
   available. Use [[cherrypick-into-release]] with the selected release branch
   and commit to open the backport PR. After it merges, refresh the refs, select
   the candidate again, and check the new tip for tags.
3. With an untagged tip, recheck that the candidate name is available and
   create an annotated tag at the exact SHA just inspected. If the name was
   claimed in the meantime, refresh the release state and recalculate.
   Preserve every existing tag.

Report the previous version, release branch, new tag, and commit SHA. By
default the branch and tag are local. Push them only when the maintainer
requests publication to the original repository. Verify that the tag produced
a GitHub prerelease and return its URL. Once validation is complete, use
[[promote-release]] to tag that candidate as stable.
