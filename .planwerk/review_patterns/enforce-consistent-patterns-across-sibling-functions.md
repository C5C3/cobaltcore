# Review Pattern: Enforce consistent patterns across sibling functions

**Review-Area**: validation
**Detection-Hint**: When a new function follows the same structure as existing sibling functions (same loop+summary pattern, same counter logic), diff the control flow against the existing functions. Any deviation in branching (e.g., `else` vs `elif`) is a red flag.
**Severity**: WARNING
**Occurrences**: 4

## What to check

When reviewing a function that mirrors the structure of nearby functions in the same file, compare the branching and counter logic line-by-line. If sibling functions use `elif [ -z "$var" ]` but the new function uses a bare `else`, flag the inconsistency and verify correctness.

## Why it matters

Inconsistency between sibling functions that follow the same pattern is a strong signal of a copy-paste bug. The reviewer already had correct reference implementations in the same file — the bug was catchable by simple comparison.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: Both the `test_sbom_format_cyclonedx_json` and `test_sbom_attestation_push_to_registry` functions correctly avoid this by using `elif [ -z "$base_formats" ]` / `elif [ -z "$base_push_values" ]` instead of a plain `else`, so only the truly unhandled "no results" case increments an extra FAIL. This function should follow the same pattern.
- **What was missed**: When reviewing a function that mirrors the structure of nearby functions in the same file, compare the branching and counter logic line-by-line. If sibling functions use `elif [ -z "$var" ]` but the new function uses a bare `else`, flag the inconsistency and verify correctness.
- **Fix**: Aligned both the `base_sbom_ifs` and `service_sbom_ifs` blocks to use the same `elif [ -z ... ]` pattern as the other test functions in the file.

### CC-0011 — greptile-apps[bot]
- **Feedback**: `CacheSpec` carries the same design intent — the `types.go` comment reads *"Exactly one of ClusterRef or Servers must be set"* — but neither the CRD schema nor the webhook's `validate()` enforces this.
- **What was missed**: For each field that documents a mutual-exclusivity or choice constraint (in comments or referenced type docs), verify that (1) an XValidation CEL rule exists on the CRD field, and (2) the webhook validate() function has a matching defence-in-depth check. Compare against sibling fields in the same spec that already have such rules.
- **Fix**: Added `+kubebuilder:validation:XValidation:rule="has(self.clusterRef) != has(self.servers)"` marker on the Cache field and a corresponding XOR check in the webhook validate() function, mirroring the existing Database field pattern.

### CC-0018 — berendt
- **Feedback**: setup-envtest and controller-gen are installed at @latest in the test-integration and verify-codegen jobs respectively. A breaking release in either tool will silently start failing CI without any diff to review. All other tooling in the workflow uses pinned versions.
- **What was missed**: All tool installations in CI (go install, pip install, npm install, etc.) must use pinned versions. No @latest. Version constants should be defined in a single location (env block or Makefile variable).
- **Fix**: Pin both tools to specific versions (e.g. controller-gen@v0.17.3, setup-envtest@v0.20.4) and define those version constants in a top-level env: block or Makefile variable for single-source-of-truth maintenance.

### CC-0046 — berendt
- **Feedback**: The version constraint `>=2.6.0 <4.0.0` spans both 2.x and 3.x major versions, unlike every other HelmRelease in the repo which pins within a single major version.
- **What was missed**: Does the new version constraint follow the same major-version pinning strategy as every other resource of the same kind in the repo? If existing HelmReleases all pin within a single major (e.g., >=X.Y.Z <X+1.0.0), a new one spanning two majors (>=2.6.0 <4.0.0) is an outlier that risks auto-adopting breaking changes.
- **Fix**: Version constraint tightened from `>=2.6.0 <4.0.0` to `>=2.6.0 <3.0.0`.
