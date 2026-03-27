# Review Pattern: Verify CI tool dependencies are explicitly installed

**Review-Area**: validation
**Detection-Hint**: For every shell command in a CI workflow step, check whether the binary is pre-installed on the runner image. Look for tools like yq, jq, helm, etc. that are NOT part of the default ubuntu-latest image.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Every binary invoked in a workflow `run:` block must either be installed in a prior step or be guaranteed available on the runner. Check the runner's pre-installed tool list (actions/runner-images docs) against what the job uses.

## Why it matters

The job will fail at runtime with 'command not found'. This is a silent time bomb — it passes review but breaks on first real execution.

## Examples from external reviews

### CC-0034 — sourcery-ai[bot]
- **Feedback**: The `Resolve source ref` step assumes `yq` is available, but this job doesn't install it. On a fresh `ubuntu-latest` runner, `yq` is not installed by default.
- **What was missed**: Every binary invoked in a workflow `run:` block must either be installed in a prior step or be guaranteed available on the runner. Check the runner's pre-installed tool list (actions/runner-images docs) against what the job uses.
- **Fix**: Add an explicit yq installation step (e.g. `mikefarah/yq` action or apt/snap install) before the step that calls yq.
