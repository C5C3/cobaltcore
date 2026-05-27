# Review Pattern: No issue-tracker references in shipped artifacts

**Review-Area**: documentation
**Detection-Hint**: Search added/changed Markdown and YAML for ticket IDs (e.g., CC-0xxx), 'contract change' notices, or commit-style annotations that belong in PR descriptions, not in the source tree.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Inline HTML comments or YAML comments that cite ticket numbers or describe the change itself rather than the artifact's purpose. Also flag 'contract change' / migration notes in projects without a stable release.

## Why it matters

Ticket references and change-log style commentary rot in-tree, leak process metadata into deliverables, and are meaningless once the ticket is closed. They belong in commit messages and PR descriptions.

## Examples from external reviews

### CC-0105 — gndrmnn
- **Feedback**: Remove the comment from Markdown file `<!-- CC-0105: operator targets `keystone-system` Namespace; workload (Keystone CR) stays in `openstack`. -->`
- **What was missed**: Inline HTML comments or YAML comments that cite ticket numbers or describe the change itself rather than the artifact's purpose. Also flag 'contract change' / migration notes in projects without a stable release.
- **Fix**: Removed the CC-0105 HTML comment from the markdown doc and removed lines 12-20 from chainsaw-test.yaml describing a 'contract change'.
