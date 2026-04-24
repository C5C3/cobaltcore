# Review Pattern: Avoid fmt.Sprintf for trivial concatenation

**Review-Area**: performance
**Detection-Hint**: Grep new diffs for fmt.Sprintf calls with only %s verbs and no formatting. Compare against how the same path is constructed elsewhere in the package (tests, constants).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Uses of fmt.Sprintf with a single %s substitution on hot paths where simple string concatenation would suffice and matches existing patterns in the package.

## Why it matters

fmt.Sprintf pulls in the fmt package and adds overhead for what is just a concatenation. On reconcile hot paths this compounds, and inconsistency with sibling code (e.g., unit tests using concatenation) makes grep/refactor harder.

## Examples from external reviews

### CC-0093 — sourcery-ai[bot]
- **Feedback**: In `credentialKeysPushSecret` and `fernetKeysPushSecret`, `fmt.Sprintf("openstack/keystone/%s/...", keystone.Name)` could be replaced with simple string concatenation to match the pattern used in the unit tests and avoid an unnecessary dependency on `fmt` in these hot paths.
- **What was missed**: Uses of fmt.Sprintf with a single %s substitution on hot paths where simple string concatenation would suffice and matches existing patterns in the package.
- **Fix**: Replace fmt.Sprintf("openstack/keystone/%s/...", keystone.Name) with "openstack/keystone/" + keystone.Name + "/..." to match the unit-test pattern and drop the fmt dependency on the hot path.
