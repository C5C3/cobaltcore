# Review Pattern: Regenerate auto-generated files after API type changes

**Review-Area**: validation
**Detection-Hint**: When a PR modifies API type structs (e.g., *_types.go) or types used in CRD generation, check whether the corresponding generated manifests, deepcopy files, and CRD YAMLs are also updated in the same PR.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any diff touching API struct definitions must include corresponding changes to auto-generated files (make generate / make regenerate output). If generated files are absent from the diff, the author likely forgot to run the generator.

## Why it matters

Stale generated files cause CRDs to drift from Go types, leading to runtime validation failures, missing fields in the API, or broken deployments that are hard to diagnose.

## Examples from external reviews

### CC-0013 — gndrmnn
- **Feedback**: I am pretty sure changing the API structs requires us to re-run ``make generate`` to re-generate the auto-generated files.
- **What was missed**: Any diff touching API struct definitions must include corresponding changes to auto-generated files (make generate / make regenerate output). If generated files are absent from the diff, the author likely forgot to run the generator.
- **Fix**: Ran make generate / make regenerate to update CRD manifests for both keystone_types.go and internal/common/types/types.go changes.
