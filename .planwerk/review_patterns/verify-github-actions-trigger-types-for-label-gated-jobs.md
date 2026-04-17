# Review Pattern: Verify GitHub Actions trigger types for label-gated jobs

**Review-Area**: configuration
**Detection-Hint**: When a workflow job or step is gated on a label (e.g., `contains(github.event.pull_request.labels.*.name, 'run-chaos')`), check that `on.pull_request.types` explicitly includes `labeled`. Without it, adding a label to an existing PR will never trigger the workflow.
**Severity**: CRITICAL
**Occurrences**: 1

## What to check

For every label-gated job or step in a GitHub Actions workflow, confirm that the `on.pull_request` trigger includes `types: [opened, synchronize, reopened, labeled]`. By default, GitHub Actions only fires `pull_request` on `opened`, `synchronize`, and `reopened` — not `labeled`. A missing `labeled` type silently defeats any label-based trigger feature.

## Why it matters

The label-gated feature appears correctly implemented at the job level but is non-functional because the workflow-level trigger never fires on label events. This is a silent failure — no error is reported, the job simply never runs. Manual testing typically adds the label before opening the PR (which uses `opened`), masking the bug for PRs where the label is added after creation.

## Examples from external reviews

### CC-0049 — berendt
- **Feedback**: The workflow's on.pull_request trigger does not include types: [opened, synchronize, reopened, labeled]. By default, GitHub Actions only triggers pull_request on opened, synchronize, and reopened — not labeled. Adding the run-chaos label to an existing PR will not trigger a workflow run, making the entire run-chaos label feature (REQ-007) non-functional.
- **What was missed**: For every label-gated job or step in a workflow, confirm that `on.pull_request.types` explicitly includes `labeled`. Without it, adding a label to an existing PR will never trigger the workflow, silently breaking the feature.
- **Fix**: Add `types: [opened, synchronize, reopened, labeled]` to the `on.pull_request` trigger in the workflow file.
