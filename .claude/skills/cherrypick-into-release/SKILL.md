---
name: cherrypick-into-release
description: >-
  Prompt for the exact CobaltCore release and source commit, cherry-pick the
  selected fix onto a backport branch, and open a pull request against the
  release branch. Use when a maintainer requests a backport to a release branch.
---

# Cherry-pick a fix into a CobaltCore release branch

## Select the release branch and source commit

Fetch the target repository's release branches, distribution tags, and `main`.
Show the available `release/<major>.<minor>` branches with their latest RC
tags where available. Ask for both selections in one prompt.

> Which release branch should receive the fix (for example, `release/1.5`),
> and which commit from `main` should I cherry-pick? A commit SHA or merged
> PR link will identify the change.

Wait for the maintainer's selection. When [[prerelease-cobaltcore]] already
collected both answers, use them. If a commit or release is ambiguous, show
the matching SHAs and subjects or release branches and ask which one they mean.

If the maintainer identifies the release by tag, resolve the tag to its
existing series branch and verify the tagged commit is in that branch's
history. For example, `v1.5.0-rc.9` identifies `release/1.5` as the PR base.

Resolve the source to the full commit SHA on `main`. For a squash-merged PR
use the landed squash commit. For a PR containing several landed commits,
ask which one to pick.
If the selected commit is a merge, establish which parent represents the
mainline before applying its diff.

## Apply the fix and open the PR

1. Check whether the selected fix is already on the release branch, including
   an equivalent cherry-pick with a different SHA. Check for an open backport
   PR with the same source SHA and base branch and with its head in the
   target repository. If either exists, return the landed commit or PR URL.
2. Start a topic branch such as `backport/1.5-<short-sha>` from the fetched
   tip of the chosen release branch. Use a separate worktree if the current
   checkout has changes. Apply the selected commit with `git cherry-pick -x`
   and preserve its original author and source SHA. If resolving a conflict
   requires a release-specific design decision, show the conflicting paths
   and ask the maintainer how the fix should behave.
3. Compare the resulting diff with the source change and run the checks that
   exercise it on this series. If a required check fails, report the failure
   before opening the PR.
4. Push the topic branch to the configured remote for the selected CobaltCore
   repository. If pushes there are disabled or denied, stop and report the
   blocker. Open the PR in that same repository, explicitly setting the topic
   branch as the head and the selected release branch as the base. Include
   the source SHA, original PR if available, reason for the backport, and
   validation results.

Return the PR URL, base branch, original SHA, and cherry-picked SHA. Leave
the PR open for review. After it merges, [[prerelease-cobaltcore]] can tag
the new release-branch tip.
