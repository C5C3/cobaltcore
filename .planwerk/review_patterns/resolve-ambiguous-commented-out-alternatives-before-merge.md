# Review Pattern: Resolve ambiguous commented-out alternatives before merge

**Review-Area**: documentation
**Detection-Hint**: Look for comments that present multiple options (e.g., 'v1alpha1 vs v1alpha2', 'TODO: pick one') without a clear resolution. These indicate unfinished design decisions left in the code.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Scan configuration files and manifests for comments that frame a choice between alternatives without committing to one. Any 'or' / 'vs' / 'pick' / open question in a comment is a red flag that a decision was deferred rather than made.

## Why it matters

Ambiguous configuration shipped to main creates confusion for anyone who later needs to modify or debug the setup. It signals the author wasn't sure, and downstream consumers won't be either—leading to silent misconfiguration or broken upgrades.

## Examples from external reviews

### CC-0047 — sourcery-ai[bot]
- **Feedback**: you currently leave the apiVersion choice (v1alpha2 vs v1alpha1) as an open decision in comments—please resolve this ambiguity and align it explicitly with what Chainsaw expects
- **What was missed**: Scan configuration files and manifests for comments that frame a choice between alternatives without committing to one. Any 'or' / 'vs' / 'pick' / open question in a comment is a red flag that a decision was deferred rather than made.
- **Fix**: Replaced the open-ended comment with an explicit rationale: 'Chainsaw v1alpha2 Configuration is paired with v1alpha1 Test manifests. This matches the upstream happy-path and is intentional.'
