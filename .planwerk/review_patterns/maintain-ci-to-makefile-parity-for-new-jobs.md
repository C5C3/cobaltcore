# Review Pattern: Maintain CI-to-Makefile parity for new jobs

**Review-Area**: architecture
**Detection-Hint**: When a new CI job is added, check whether sibling jobs in the same workflow delegate to Makefile targets. If they do and the new job does not, flag the inconsistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Does the PR add a CI job that runs commands inline while existing peer jobs (test-race, format-check, lint) delegate to Makefile targets? Is there a missing Makefile target that would let developers reproduce the CI check locally?

## Why it matters

Without a local Makefile target, developers cannot reproduce CI failures without reading the workflow YAML and manually assembling the commands. This breaks the established pattern and increases friction for debugging CI issues locally.

## Examples from external reviews

### CC-0061 — berendt
- **Feedback**: The test-race and format-check CI jobs each have a corresponding Makefile target. The govulncheck CI job has no Makefile counterpart. Developers must manually run go install and three separate govulncheck commands to replicate CI locally.
- **What was missed**: Does the PR add a CI job that runs commands inline while existing peer jobs (test-race, format-check, lint) delegate to Makefile targets? Is there a missing Makefile target that would let developers reproduce the CI check locally?
- **Fix**: Added a `make govulncheck` target and updated the CI step to call `make govulncheck` instead of inline commands.
