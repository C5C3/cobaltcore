# Review Pattern: Verify documentation headings match actual dependency graph

**Review-Area**: documentation
**Detection-Hint**: When CI workflow documentation groups jobs under a heading that implies a specific trigger mechanism (e.g., 'path-filtered'), cross-reference each listed job against the actual workflow YAML to confirm it truly uses that mechanism.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For each job listed under a conditional/filtered section in CI docs, verify the job actually has the stated dependency (needs, if-condition, path filter). Any job that doesn't match the heading's description must be moved to its own section.

## Why it matters

Misleading DAG documentation causes engineers to misunderstand which jobs are gated and which run unconditionally, leading to incorrect assumptions about CI behavior and wasted debugging time.

## Examples from external reviews

### CC-0041 — berendt
- **Feedback**: The docs job is placed under the heading 'Conditional Jobs (path-filtered via changes job)' but it does NOT depend on the changes job and is NOT path-filtered. This is inaccurate and misleading.
- **What was missed**: For each job listed under a conditional/filtered section in CI docs, verify the job actually has the stated dependency (needs, if-condition, path filter). Any job that doesn't match the heading's description must be moved to its own section.
- **Fix**: Split the section so path-filtered jobs stay under 'Conditional Jobs' and the docs job gets its own 'Independent Jobs' heading.
